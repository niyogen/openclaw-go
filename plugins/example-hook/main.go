// openclaw-go reference hook plugin.
//
// Subscribes to nine gateway lifecycle events and logs the envelope each
// time the gateway fires one. Useful as a probe ("did my event fire?")
// and as a working template for real audit / notification plugins.
//
// Build:
//
//	go build -o example-hook ./plugins/example-hook   (Linux/macOS)
//	go build -o example-hook.exe ./plugins/example-hook   (Windows)
//
// Run (env vars come from the gateway after `openclaw plugins hook approve example-hook`):
//
//	OPENCLAW_PLUGIN_NAME=example-hook \
//	OPENCLAW_GATEWAY_URL=http://127.0.0.1:18789 \
//	OPENCLAW_PLUGIN_TOKEN=<paste-from-approve> \
//	./example-hook
//
// The plugin listens on $OPENCLAW_PLUGIN_ADDR (default :9301), matching
// the endpoint URLs in plugin.json. Per the design contract hooks are
// fire-and-forget — this plugin always returns 2xx and logs to stderr.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"

	"openclaw-go/pkg/hookplugin"
)

// subscriptions maps the URL path the manifest declares to a friendly
// label used in log output. Keep these aligned with plugin.json — the
// path is the join key.
var subscriptions = map[string]string{
	"/hook/gateway-started":    "gateway.started",
	"/hook/gateway-stopping":   "gateway.stopping",
	"/hook/session-created":    "session.created",
	"/hook/message-received":   "message.received",
	"/hook/message-sent":       "message.sent",
	"/hook/agent-run-started":  "agent.run.started",
	"/hook/agent-run-complete": "agent.run.complete",
	"/hook/tool-invoked":       "tool.invoked",
	"/hook/approval-requested": "approval.requested",
}

func main() {
	plugin, err := hookplugin.LoadFromEnv()
	if err != nil {
		log.Fatalf("[example-hook] %v", err)
	}

	for path, label := range subscriptions {
		plugin.HandlePath(path, func(_ context.Context, env hookplugin.Envelope) {
			payload, _ := json.Marshal(env.Payload)
			log.Printf("[example-hook] event=%s ts=%s payload=%s", label, env.Timestamp, string(payload))
		})
	}

	addr := strings.TrimSpace(os.Getenv("OPENCLAW_PLUGIN_ADDR"))
	if addr == "" {
		addr = ":9301"
	}
	log.Printf("[example-hook] listening on %s (gateway=%s)", addr, plugin.GatewayURL)
	log.Printf("[example-hook] subscribed paths: %v", plugin.Paths())
	log.Fatal(plugin.Listen(addr))
}

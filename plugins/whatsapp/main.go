// openclaw-go WhatsApp Cloud API channel plugin.
//
// Migrated from the in-process channel at internal/channels/whatsapp.go.
// The plugin reuses the same WhatsAppChannel code as a library (Path α
// per docs/PLUGIN-ARCHITECTURE.md "config sourcing") and wraps it in the
// SDK contract: gateway POSTs outbound at /channel/send.
//
// v1 scope is OUTBOUND ONLY. WhatsApp inbound is webhook-only — Meta's
// signed POST hits a public URL — and that webhook handler continues to
// live in the gateway under cfg.Channels.WhatsApp.WebhookPath. The
// gateway-side UsePlugin gate only skips the OUTBOUND register block,
// not the inbound webhook handler, so a mid-migration deployment keeps
// receiving messages while outbound delivery moves out-of-process.
//
// Operator launches this binary with three env vars set:
//
//	OPENCLAW_PLUGIN_NAME    must equal manifest.name ("whatsapp")
//	OPENCLAW_GATEWAY_URL    e.g. http://127.0.0.1:18789
//	OPENCLAW_PLUGIN_TOKEN   from `openclaw plugins channel approve whatsapp`
//
// Plus the standard openclaw config-path env so this plugin can read
// channels.whatsapp.{accessToken,phoneNumberId,toNumber,...}:
//
//	OPENCLAW_CONFIG_PATH    optional; defaults to ~/.openclaw-go/openclaw.json
//
// The plugin listens on $OPENCLAW_PLUGIN_ADDR (default :9102).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("[whatsapp-plugin] %v", err)
	}
}

func run() error {
	cfg, err := buildConfig()
	if err != nil {
		return err
	}
	addr := strings.TrimSpace(os.Getenv("OPENCLAW_PLUGIN_ADDR"))
	if addr == "" {
		addr = ":9102"
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	plugin, err := newWhatsAppPlugin(cfg)
	if err != nil {
		return err
	}

	fmt.Printf("[whatsapp-plugin] listening on %s (gateway=%s)\n", addr, cfg.gatewayURL)
	return plugin.listen(ctx, addr)
}

// openclaw-go reference tool plugin.
//
// Three tools that together demonstrate the moving parts plugin authors
// need to know:
//
//	example_echo — returns args as the result (smoke test the wiring)
//	example_now  — takes no args, returns RFC3339 timestamp
//	example_add  — typed args + error path
//
// Build:
//
//	go build -o example-tool ./plugins/example-tool   (Linux/macOS)
//	go build -o example-tool.exe ./plugins/example-tool   (Windows)
//
// Run (env vars come from the gateway after `openclaw plugins tool approve example-tool`):
//
//	OPENCLAW_PLUGIN_NAME=example-tool \
//	OPENCLAW_GATEWAY_URL=http://127.0.0.1:18789 \
//	OPENCLAW_PLUGIN_TOKEN=<paste-from-approve> \
//	./example-tool
//
// The plugin listens on $OPENCLAW_PLUGIN_ADDR (default :9201), matching
// the endpoint URLs in plugin.json.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"openclaw-go/pkg/toolplugin"
)

func main() {
	plugin, err := toolplugin.LoadFromEnv()
	if err != nil {
		log.Fatalf("[example-tool] %v", err)
	}

	plugin.RegisterTool("example_echo", echoHandler)
	plugin.RegisterTool("example_now", nowHandler)
	plugin.RegisterTool("example_add", addHandler)

	addr := strings.TrimSpace(os.Getenv("OPENCLAW_PLUGIN_ADDR"))
	if addr == "" {
		addr = ":9201"
	}
	log.Printf("[example-tool] listening on %s (gateway=%s)", addr, plugin.GatewayURL)
	log.Printf("[example-tool] registered tools: %v", plugin.Tools())
	log.Fatal(plugin.Listen(addr))
}

// echoHandler returns the args map verbatim. Useful as a "is the gateway
// actually talking to me?" probe.
func echoHandler(_ context.Context, args map[string]any) (any, error) {
	if args == nil {
		args = map[string]any{}
	}
	return map[string]any{
		"echoed": args,
		"count":  len(args),
	}, nil
}

// nowHandler ignores args and returns the plugin's current UTC time.
// Demonstrates an argument-free tool.
func nowHandler(_ context.Context, _ map[string]any) (any, error) {
	return map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

// addHandler sums "a" and "b". Demonstrates input validation + the
// error path — when args are missing or non-numeric, the gateway sees
// a structured failure rather than a panic.
func addHandler(_ context.Context, args map[string]any) (any, error) {
	a, errA := asFloat(args, "a")
	b, errB := asFloat(args, "b")
	if errA != nil {
		return nil, errA
	}
	if errB != nil {
		return nil, errB
	}
	return map[string]any{"sum": a + b}, nil
}

func asFloat(args map[string]any, key string) (float64, error) {
	v, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("missing required arg %q", key)
	}
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case string:
		// JSON-RPC clients sometimes stringify numerics; accept that.
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%g", &f); err == nil {
			return f, nil
		}
		return 0, fmt.Errorf("arg %q is a non-numeric string %q", key, n)
	default:
		return 0, errors.New("arg " + key + " is not a number")
	}
}

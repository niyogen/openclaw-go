// openclaw-go Telegram channel plugin.
//
// Migrated from the in-process channel at internal/channels/telegram.go.
// The plugin reuses the same TelegramChannel + TelegramPoller code as a
// library (Path α per docs/PLUGIN-ARCHITECTURE.md "config sourcing") and
// wraps them in the SDK contract: gateway POSTs outbound at /channel/send,
// plugin POSTs inbound back to the gateway's /plugins/{name}/inbound.
//
// Operator launches this binary with three env vars set:
//
//	OPENCLAW_PLUGIN_NAME    must equal manifest.name ("telegram")
//	OPENCLAW_GATEWAY_URL    e.g. http://127.0.0.1:18789
//	OPENCLAW_PLUGIN_TOKEN   from `openclaw plugins channel approve telegram`
//
// Plus the standard openclaw config-path env so this plugin can read
// channels.telegram.{botToken,chatId,inboundMode,...}:
//
//	OPENCLAW_CONFIG_PATH    optional; defaults to ~/.openclaw-go/openclaw.json
//
// The plugin listens on $OPENCLAW_PLUGIN_ADDR (default :9101).
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
		log.Fatalf("[telegram-plugin] %v", err)
	}
}

func run() error {
	cfg, err := buildConfig()
	if err != nil {
		return err
	}
	addr := strings.TrimSpace(os.Getenv("OPENCLAW_PLUGIN_ADDR"))
	if addr == "" {
		addr = ":9101"
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	plugin, err := newTelegramPlugin(cfg)
	if err != nil {
		return err
	}
	plugin.start(ctx)

	fmt.Printf("[telegram-plugin] listening on %s (gateway=%s)\n", addr, cfg.gatewayURL)
	return plugin.listen(ctx, addr)
}

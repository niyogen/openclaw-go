package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"openclaw-go/internal/channels"
	"openclaw-go/internal/config"
	"openclaw-go/pkg/channelplugin"
)

// pluginConfig bundles the runtime configuration the plugin needs at
// startup. Split out so tests can construct one directly without env
// gymnastics.
type pluginConfig struct {
	name          string // OPENCLAW_PLUGIN_NAME
	gatewayURL    string // OPENCLAW_GATEWAY_URL
	token         string // OPENCLAW_PLUGIN_TOKEN
	botToken      string // channels.telegram.botToken
	chatID        string // channels.telegram.chatId (default outbound target)
	inboundMode   string // "polling" | "webhook" | ""(=polling)
	webhookSecret string // not used by polling mode; kept for future webhook-mode work
}

// buildConfig reads the three required SDK env vars + opens the
// operator's openclaw.json to extract the channels.telegram subtree.
// Path α per design note: plugin reuses the gateway's config schema.
func buildConfig() (pluginConfig, error) {
	name := strings.TrimSpace(os.Getenv("OPENCLAW_PLUGIN_NAME"))
	gw := strings.TrimSpace(os.Getenv("OPENCLAW_GATEWAY_URL"))
	tok := strings.TrimSpace(os.Getenv("OPENCLAW_PLUGIN_TOKEN"))
	if name == "" || gw == "" || tok == "" {
		return pluginConfig{}, fmt.Errorf("missing required env vars OPENCLAW_PLUGIN_NAME/OPENCLAW_GATEWAY_URL/OPENCLAW_PLUGIN_TOKEN")
	}

	cfg, err := config.Load("") // honors OPENCLAW_CONFIG_PATH
	if err != nil {
		return pluginConfig{}, fmt.Errorf("load openclaw.json: %w", err)
	}
	tg := cfg.Channels.Telegram
	if strings.TrimSpace(tg.BotToken) == "" {
		return pluginConfig{}, errors.New("channels.telegram.botToken is empty — set it via `openclaw configure telegram` or env")
	}

	mode := strings.ToLower(strings.TrimSpace(tg.InboundMode))
	if mode == "" {
		mode = "polling"
	}

	return pluginConfig{
		name:          name,
		gatewayURL:    gw,
		token:         tok,
		botToken:      strings.TrimSpace(tg.BotToken),
		chatID:        strings.TrimSpace(tg.ChatID),
		inboundMode:   mode,
		webhookSecret: tg.WebhookSecret,
	}, nil
}

// telegramPlugin owns the SDK Plugin handle, the outbound TelegramChannel,
// and (in polling mode) a TelegramPoller. start spawns the inbound
// goroutine; listen runs the HTTP server until ctx is cancelled.
type telegramPlugin struct {
	cfg      pluginConfig
	plugin   *channelplugin.Plugin
	outbound *channels.TelegramChannel
	poller   *channels.TelegramPoller // nil in webhook mode
	server   *http.Server
}

func newTelegramPlugin(cfg pluginConfig) (*telegramPlugin, error) {
	outbound := channels.NewTelegramChannel(cfg.botToken, cfg.chatID)
	plugin := &channelplugin.Plugin{
		Name:       cfg.name,
		GatewayURL: cfg.gatewayURL,
		Token:      cfg.token,
	}
	plugin.OnSend = func(ctx context.Context, msg channelplugin.OutboundMessage) error {
		// Convert the SDK type to the channels package type that
		// TelegramChannel.Send expects. Field-for-field copy — the
		// JSON tags match, so the round-trip via gateway → plugin
		// preserves everything.
		return outbound.Send(ctx, convertOutbound(msg))
	}
	tp := &telegramPlugin{
		cfg:      cfg,
		plugin:   plugin,
		outbound: outbound,
	}
	return tp, nil
}

// start launches the inbound poller in polling mode. Webhook mode is
// deferred — operators still using webhook should keep the built-in
// path until plugin webhook support lands.
func (tp *telegramPlugin) start(ctx context.Context) {
	if tp.cfg.inboundMode != "polling" {
		// Webhook mode: leave the existing built-in gateway path to
		// handle inbound. Outbound still flows through this plugin.
		return
	}
	tp.poller = channels.NewTelegramPoller(tp.cfg.botToken)
	tp.poller.Start(ctx, func(pctx context.Context, inbound channels.InboundMessage) error {
		return tp.plugin.DispatchInbound(pctx, convertInbound(inbound))
	}, nil)
}

// listen serves /channel/send on addr until ctx is cancelled. Uses the
// SDK's Handler so the server contract stays consistent across plugins.
func (tp *telegramPlugin) listen(ctx context.Context, addr string) error {
	tp.server = &http.Server{
		Addr:              addr,
		Handler:           tp.plugin.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		err := tp.server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.server.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

func convertOutbound(m channelplugin.OutboundMessage) channels.OutboundMessage {
	return channels.OutboundMessage{
		SessionID:        m.SessionID,
		Channel:          m.Channel,
		Target:           m.Target,
		Message:          m.Message,
		ThreadID:         m.ThreadID,
		ReplyToMessageID: m.ReplyToMessageID,
		MediaURL:         m.MediaURL,
		Buttons:          convertButtons(m.Buttons),
		Reactions:        convertReactions(m.Reactions),
		Ephemeral:        m.Ephemeral,
	}
}

func convertInbound(m channels.InboundMessage) channelplugin.InboundMessage {
	return channelplugin.InboundMessage{
		SessionID: m.SessionID,
		Channel:   m.Channel,
		Target:    m.Target,
		Message:   m.Message,
	}
}

func convertButtons(in []channelplugin.Button) []channels.Button {
	if len(in) == 0 {
		return nil
	}
	out := make([]channels.Button, len(in))
	for i, b := range in {
		out[i] = channels.Button{Label: b.Label, Value: b.Value, Style: b.Style, Action: b.Action}
	}
	return out
}

func convertReactions(in []channelplugin.Reaction) []channels.Reaction {
	if len(in) == 0 {
		return nil
	}
	out := make([]channels.Reaction, len(in))
	for i, r := range in {
		out[i] = channels.Reaction{Emoji: r.Emoji, MessageID: r.MessageID}
	}
	return out
}

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
	accessToken   string // channels.whatsapp.accessToken
	phoneNumberID string // channels.whatsapp.phoneNumberId
	toNumber      string // channels.whatsapp.toNumber (default outbound target)
}

// buildConfig reads the three required SDK env vars + opens the
// operator's openclaw.json to extract the channels.whatsapp subtree.
// Path α per design note: plugin reuses the gateway's config schema.
//
// WhatsApp inbound is webhook-only (Meta-driven), so unlike Telegram
// there is no inboundMode to honor here — the gateway continues to own
// the inbound webhook in v1.
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
	wa := cfg.Channels.WhatsApp
	if strings.TrimSpace(wa.AccessToken) == "" {
		return pluginConfig{}, errors.New("channels.whatsapp.accessToken is empty — set it via env WHATSAPP_ACCESS_TOKEN or in openclaw.json")
	}
	if strings.TrimSpace(wa.PhoneNumberID) == "" {
		return pluginConfig{}, errors.New("channels.whatsapp.phoneNumberId is empty — set it via env WHATSAPP_PHONE_NUMBER_ID or in openclaw.json")
	}

	return pluginConfig{
		name:          name,
		gatewayURL:    gw,
		token:         tok,
		accessToken:   strings.TrimSpace(wa.AccessToken),
		phoneNumberID: strings.TrimSpace(wa.PhoneNumberID),
		toNumber:      strings.TrimSpace(wa.ToNumber),
	}, nil
}

// whatsappPlugin owns the SDK Plugin handle and the outbound
// WhatsAppChannel. No poller — WhatsApp is webhook-only and the
// gateway continues to own the webhook handler in v1 scope.
type whatsappPlugin struct {
	cfg      pluginConfig
	plugin   *channelplugin.Plugin
	outbound *channels.WhatsAppChannel
	server   *http.Server
}

func newWhatsAppPlugin(cfg pluginConfig) (*whatsappPlugin, error) {
	outbound := channels.NewWhatsAppChannel(cfg.accessToken, cfg.phoneNumberID, cfg.toNumber)
	plugin := &channelplugin.Plugin{
		Name:       cfg.name,
		GatewayURL: cfg.gatewayURL,
		Token:      cfg.token,
	}
	plugin.OnSend = func(ctx context.Context, msg channelplugin.OutboundMessage) error {
		// Convert the SDK type to the channels package type that
		// WhatsAppChannel.Send expects. Field-for-field copy — the
		// JSON tags match, so the round-trip via gateway → plugin
		// preserves everything.
		return outbound.Send(ctx, convertOutbound(msg))
	}
	wp := &whatsappPlugin{
		cfg:      cfg,
		plugin:   plugin,
		outbound: outbound,
	}
	return wp, nil
}

// listen serves /channel/send on addr until ctx is cancelled. Uses the
// SDK's Handler so the server contract stays consistent across plugins.
func (wp *whatsappPlugin) listen(ctx context.Context, addr string) error {
	wp.server = &http.Server{
		Addr:              addr,
		Handler:           wp.plugin.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		err := wp.server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = wp.server.Shutdown(shutdownCtx)
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

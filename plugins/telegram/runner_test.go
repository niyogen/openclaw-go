package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openclaw-go/internal/channels"
	"openclaw-go/internal/config"
	"openclaw-go/pkg/channelplugin"
)

// writeConfigToTemp writes an openclaw.json with the given Telegram block
// to a temp file and points OPENCLAW_CONFIG_PATH at it. Used by buildConfig
// tests so they don't touch the operator's real config.
func writeConfigToTemp(t *testing.T, tg config.TelegramChannelConfig) {
	t.Helper()
	cfg := config.Default()
	cfg.Channels.Telegram = tg
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCLAW_CONFIG_PATH", path)
}

func TestBuildConfigHappyPath(t *testing.T) {
	writeConfigToTemp(t, config.TelegramChannelConfig{
		Enabled:     true,
		BotToken:    "test-bot-token",
		ChatID:      "12345",
		InboundMode: "polling",
	})
	t.Setenv("OPENCLAW_PLUGIN_NAME", "telegram")
	t.Setenv("OPENCLAW_GATEWAY_URL", "http://127.0.0.1:18789")
	t.Setenv("OPENCLAW_PLUGIN_TOKEN", "tok-xyz")

	cfg, err := buildConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.botToken != "test-bot-token" || cfg.chatID != "12345" {
		t.Fatalf("config mismatch: %+v", cfg)
	}
	if cfg.inboundMode != "polling" {
		t.Errorf("inbound mode: %q", cfg.inboundMode)
	}
}

func TestBuildConfigDefaultsModeToPolling(t *testing.T) {
	writeConfigToTemp(t, config.TelegramChannelConfig{
		BotToken:    "test",
		InboundMode: "", // empty
	})
	t.Setenv("OPENCLAW_PLUGIN_NAME", "telegram")
	t.Setenv("OPENCLAW_GATEWAY_URL", "http://x")
	t.Setenv("OPENCLAW_PLUGIN_TOKEN", "t")

	cfg, err := buildConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.inboundMode != "polling" {
		t.Errorf("empty mode should default to polling; got %q", cfg.inboundMode)
	}
}

func TestBuildConfigErrorsOnMissingBotToken(t *testing.T) {
	writeConfigToTemp(t, config.TelegramChannelConfig{
		BotToken: "", // missing
	})
	t.Setenv("OPENCLAW_PLUGIN_NAME", "telegram")
	t.Setenv("OPENCLAW_GATEWAY_URL", "http://x")
	t.Setenv("OPENCLAW_PLUGIN_TOKEN", "t")

	if _, err := buildConfig(); err == nil {
		t.Fatal("expected error when bot token is empty")
	}
}

func TestBuildConfigErrorsOnMissingSDKEnv(t *testing.T) {
	writeConfigToTemp(t, config.TelegramChannelConfig{BotToken: "x"})
	// Don't set any of the SDK env vars.
	t.Setenv("OPENCLAW_PLUGIN_NAME", "")
	t.Setenv("OPENCLAW_GATEWAY_URL", "")
	t.Setenv("OPENCLAW_PLUGIN_TOKEN", "")

	if _, err := buildConfig(); err == nil {
		t.Fatal("expected error when SDK env vars are unset")
	}
}

// ──────────────────────────────────────────────────────────────────────
// Conversion helpers — preserve shape across the SDK / channels boundary.
// ──────────────────────────────────────────────────────────────────────

func TestConvertOutboundPreservesAllFields(t *testing.T) {
	in := channelplugin.OutboundMessage{
		SessionID:        "s1",
		Channel:          "telegram",
		Target:           "12345",
		Message:          "hi",
		ThreadID:         "thread-1",
		ReplyToMessageID: "reply-1",
		MediaURL:         "https://example.com/img.png",
		Buttons:          []channelplugin.Button{{Label: "OK", Value: "ok", Style: "primary"}},
		Reactions:        []channelplugin.Reaction{{Emoji: "+1", MessageID: "m1"}},
		Ephemeral:        true,
	}
	got := convertOutbound(in)
	if got.SessionID != "s1" || got.Target != "12345" || got.Message != "hi" {
		t.Errorf("core fields lost: %+v", got)
	}
	if got.ThreadID != "thread-1" || got.ReplyToMessageID != "reply-1" {
		t.Errorf("threading fields lost: %+v", got)
	}
	if got.MediaURL != "https://example.com/img.png" {
		t.Errorf("media url lost: %q", got.MediaURL)
	}
	if len(got.Buttons) != 1 || got.Buttons[0].Label != "OK" {
		t.Errorf("buttons not converted: %+v", got.Buttons)
	}
	if len(got.Reactions) != 1 || got.Reactions[0].Emoji != "+1" {
		t.Errorf("reactions not converted: %+v", got.Reactions)
	}
	if !got.Ephemeral {
		t.Error("ephemeral flag lost")
	}
}

func TestConvertInboundPreservesFields(t *testing.T) {
	in := channels.InboundMessage{
		SessionID: "telegram:42",
		Channel:   "telegram",
		Target:    "42",
		Message:   "hello",
	}
	got := convertInbound(in)
	if got.SessionID != "telegram:42" || got.Channel != "telegram" ||
		got.Target != "42" || got.Message != "hello" {
		t.Fatalf("inbound conversion lost data: %+v", got)
	}
}

// ──────────────────────────────────────────────────────────────────────
// Plugin HTTP contract — exercises /channel/send via the SDK Handler.
// Uses a stub OnSend so we don't hit the real Telegram API; the underlying
// TelegramChannel.Send is already covered by internal/channels/telegram_test.go.
// ──────────────────────────────────────────────────────────────────────

// newTestPlugin constructs a telegramPlugin with a stub OnSend, bypassing
// the real Telegram client. The plugin's HTTP server still mounts the
// SDK's /channel/send handler — we're verifying the wiring, not the Bot
// API client itself.
func newTestPlugin(t *testing.T, onSend func(context.Context, channelplugin.OutboundMessage) error) *telegramPlugin {
	t.Helper()
	tp := &telegramPlugin{
		cfg: pluginConfig{
			name: "telegram", gatewayURL: "http://x", token: "tok",
			botToken: "bot", chatID: "12345", inboundMode: "polling",
		},
		plugin: &channelplugin.Plugin{
			Name:       "telegram",
			GatewayURL: "http://x",
			Token:      "tok",
			OnSend:     onSend,
		},
	}
	return tp
}

func TestPluginHandlerDispatchesOnSend(t *testing.T) {
	var seen channelplugin.OutboundMessage
	tp := newTestPlugin(t, func(_ context.Context, msg channelplugin.OutboundMessage) error {
		seen = msg
		return nil
	})
	srv := httptest.NewServer(tp.plugin.Handler())
	t.Cleanup(srv.Close)

	body := `{"sessionId":"s1","channel":"telegram","target":"42","message":"hi"}`
	resp, err := http.Post(srv.URL+"/channel/send", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if seen.Target != "42" || seen.Message != "hi" {
		t.Fatalf("OnSend got: %+v", seen)
	}
}

func TestPluginHandlerSurfacesOnSendError(t *testing.T) {
	tp := newTestPlugin(t, func(_ context.Context, _ channelplugin.OutboundMessage) error {
		return errors.New("Telegram API said no")
	})
	srv := httptest.NewServer(tp.plugin.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/channel/send", "application/json",
		strings.NewReader(`{"message":"x","target":"y"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 when OnSend errors; got %d", resp.StatusCode)
	}
}

// ──────────────────────────────────────────────────────────────────────
// Inbound polling: verifies the plugin's poller dispatches messages back
// to the gateway via Plugin.DispatchInbound.
// ──────────────────────────────────────────────────────────────────────

func TestInboundDispatchPostsToGateway(t *testing.T) {
	// Stand up a fake gateway that captures POSTs to /plugins/telegram/inbound.
	var seenAuth, seenPath, seenBody string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenPath = r.URL.Path
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		seenBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(gw.Close)

	plugin := &channelplugin.Plugin{
		Name:       "telegram",
		GatewayURL: gw.URL,
		Token:      "tok-abc",
	}
	// Direct invocation — same code path the production poller takes.
	err := plugin.DispatchInbound(context.Background(), channelplugin.InboundMessage{
		SessionID: "telegram:99",
		Channel:   "telegram",
		Target:    "99",
		Message:   "inbound test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenAuth != "Bearer tok-abc" {
		t.Errorf("auth: %q", seenAuth)
	}
	if seenPath != "/plugins/telegram/inbound" {
		t.Errorf("path: %q", seenPath)
	}
	var got channelplugin.InboundMessage
	_ = json.Unmarshal([]byte(seenBody), &got)
	if got.Message != "inbound test" || got.SessionID != "telegram:99" {
		t.Errorf("body: %+v", got)
	}
}

// ──────────────────────────────────────────────────────────────────────
// Listen lifecycle: the plugin's HTTP server starts + shuts down cleanly
// when ctx is cancelled.
// ──────────────────────────────────────────────────────────────────────

func TestListenShutsDownOnCtxCancel(t *testing.T) {
	// What this test pins: listen() returns nil within a few seconds
	// after ctx is cancelled. We deliberately do NOT inspect tp.server
	// from the test goroutine — that's a field written inside listen()'s
	// goroutine, and a defensive "is it initialized" peek was flagged
	// as a data race by CI. The shutdown semantic is the contract;
	// the field layout is an implementation detail.
	tp := newTestPlugin(t, func(_ context.Context, _ channelplugin.OutboundMessage) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())

	listenDone := make(chan error, 1)
	go func() {
		listenDone <- tp.listen(ctx, "127.0.0.1:0") // ephemeral port
	}()

	// Give the server a moment to bind, then cancel and assert shutdown.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case err := <-listenDone:
		if err != nil {
			t.Fatalf("listen returned error on shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("listen did not shut down within 3s of ctx cancel")
	}
}

// ──────────────────────────────────────────────────────────────────────
// Webhook-mode noop: when inboundMode != "polling", start() must NOT
// spawn a poller. (Otherwise we'd double-poll alongside the gateway's
// built-in webhook handler in deployments mid-migration.)
// ──────────────────────────────────────────────────────────────────────

func TestStartSkipsPollerInWebhookMode(t *testing.T) {
	cfg := pluginConfig{
		name: "telegram", gatewayURL: "http://x", token: "t",
		botToken: "bot", inboundMode: "webhook",
	}
	tp, err := newTelegramPlugin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tp.start(ctx)
	if tp.poller != nil {
		t.Fatal("webhook mode should not spawn the poller")
	}
}

// Diagnostic helper for failures; not strictly necessary but useful when
// tests are run with -v.
var _ = fmt.Sprint(bytes.NewReader(nil))

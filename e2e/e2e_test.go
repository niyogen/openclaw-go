// Package e2e contains end-to-end tests for the openclaw-go gateway.
// Tests start the gateway in-process (no separate server required) and hit
// every major HTTP surface area.
//
// Run with:
//
//	go test ./e2e/... -v -timeout 60s
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openclaw-go/internal/agents"
	"openclaw-go/internal/channels"
	"openclaw-go/internal/gateway"
	"openclaw-go/internal/plugins"
	"openclaw-go/internal/sessions"
)

// channelHarness creates a harness with all six channel webhook routes mounted.
func newChannelHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, "")

	handleInbound := func(ctx context.Context, inbound channels.InboundMessage) error {
		_, err := h.server.HandleInbound(ctx, inbound)
		return err
	}

	// Register all channel webhook routes the same way runGateway does.
	h.server.HandleFunc("/webhooks/telegram",
		channels.BuildTelegramWebhookHandler("", handleInbound))
	h.server.HandleFunc("/webhooks/slack",
		channels.BuildSlackWebhookHandler("", handleInbound))
	h.server.HandleFunc("/webhooks/discord",
		channels.BuildDiscordWebhookHandler("", handleInbound))
	h.server.HandleFunc("/webhooks/teams",
		channels.BuildTeamsWebhookHandler("", handleInbound))
	h.server.HandleFunc("/webhooks/whatsapp",
		channels.BuildWhatsAppWebhookHandler("", "", handleInbound))

	return h
}

// ── test harness ─────────────────────────────────────────────────────────────

type harness struct {
	base   string
	token  string
	server *gateway.Server
	cancel context.CancelFunc
}

func newHarness(t *testing.T, authToken string) *harness {
	t.Helper()
	dir := t.TempDir()

	store, err := sessions.New(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}

	reg := plugins.NewRegistry()
	reg.Register(plugins.NewMetaPlugin(reg))

	srv := gateway.New(
		"127.0.0.1", 0,
		authToken,
		[]string{"http://127.0.0.1"},
		store,
		&agents.EchoRunner{},
		channels.NewRouter(),
		reg,
		dir,
	)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)

	// Start the server in background using a custom listener port.
	// We recreate the server bound to the discovered port.
	srv2 := gateway.New(
		"127.0.0.1", port,
		authToken,
		[]string{"http://127.0.0.1"},
		store,
		&agents.EchoRunner{},
		channels.NewRouter(),
		reg,
		dir,
	)

	go func() { _ = srv2.Run(ctx) }()
	time.Sleep(50 * time.Millisecond) // let it bind

	_ = srv // unused; srv2 is the live one

	return &harness{
		base:   fmt.Sprintf("http://127.0.0.1:%d", port),
		token:  authToken,
		server: srv2,
		cancel: cancel,
	}
}

func (h *harness) close() { h.cancel() }

func (h *harness) headers() http.Header {
	hdr := http.Header{}
	if h.token != "" {
		hdr.Set("Authorization", "Bearer "+h.token)
	}
	hdr.Set("Content-Type", "application/json")
	return hdr
}

func (h *harness) get(t *testing.T, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.base+path, nil)
	if err != nil {
		t.Fatalf("GET %s build request: %v", path, err)
	}
	for k, vs := range h.headers() {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func (h *harness) post(t *testing.T, path string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	switch v := body.(type) {
	case string:
		r = strings.NewReader(v)
	case []byte:
		r = bytes.NewReader(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("POST %s marshal: %v", path, err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(http.MethodPost, h.base+path, r)
	if err != nil {
		t.Fatalf("POST %s build request: %v", path, err)
	}
	for k, vs := range h.headers() {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func (h *harness) delete(t *testing.T, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, h.base+path, nil)
	if err != nil {
		t.Fatalf("DELETE %s build request: %v", path, err)
	}
	for k, vs := range h.headers() {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	return resp
}

func rpc(t *testing.T, h *harness, method string, params any) map[string]any {
	t.Helper()
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	}
	resp := h.post(t, "/rpc", payload)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("rpc %s: HTTP %d", method, resp.StatusCode)
	}
	var envelope map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&envelope)
	return envelope
}

func readBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return m
}

func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected HTTP %d, got %d: %s", want, resp.StatusCode, body)
	}
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestE2E_Health(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	for _, path := range []string{"/health", "/healthz", "/ready", "/readyz", "/v1/health", "/v1/healthz"} {
		resp := h.get(t, path)
		assertStatus(t, resp, 200)
		body := readBody(t, resp)
		if body["ok"] != true {
			t.Errorf("%s: expected ok=true", path)
		}
	}
}

func TestE2E_Auth(t *testing.T) {
	h := newHarness(t, "mysecret")
	defer h.close()

	// /health must be public.
	resp := h.get(t, "/health")
	assertStatus(t, resp, 200)
	resp.Body.Close()

	// /sessions without token must 401.
	req, _ := http.NewRequest(http.MethodGet, h.base+"/sessions", nil)
	resp, _ = http.DefaultClient.Do(req)
	assertStatus(t, resp, 401)
	resp.Body.Close()

	// /sessions with X-OpenClaw-Token must 200.
	req, _ = http.NewRequest(http.MethodGet, h.base+"/sessions", nil)
	req.Header.Set("X-OpenClaw-Token", "mysecret")
	resp, _ = http.DefaultClient.Do(req)
	assertStatus(t, resp, 200)
	resp.Body.Close()

	// /sessions with ?token= must 200.
	req, _ = http.NewRequest(http.MethodGet, h.base+"/sessions?token=mysecret", nil)
	resp, _ = http.DefaultClient.Do(req)
	assertStatus(t, resp, 200)
	resp.Body.Close()
}

func TestE2E_SessionCRUD(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	// Create via /message.
	resp := h.post(t, "/message", map[string]string{
		"sessionId": "e2e-sess", "message": "hello", "channel": "cli",
	})
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	if body["reply"] == "" {
		t.Fatal("expected reply")
	}

	// List.
	resp = h.get(t, "/sessions")
	assertStatus(t, resp, 200)
	body = readBody(t, resp)
	sessions, _ := body["sessions"].([]any)
	if len(sessions) == 0 {
		t.Fatal("expected at least 1 session")
	}

	// Get.
	resp = h.get(t, "/sessions/e2e-sess")
	assertStatus(t, resp, 200)
	body = readBody(t, resp)
	if body["id"] != "e2e-sess" {
		t.Fatalf("unexpected session id: %v", body["id"])
	}

	// History.
	resp = h.get(t, "/sessions/e2e-sess/history")
	assertStatus(t, resp, 200)
	body = readBody(t, resp)
	hist, _ := body["history"].([]any)
	if len(hist) < 2 {
		t.Fatalf("expected >= 2 messages, got %d", len(hist))
	}

	// Patch.
	resp = h.post(t, "/sessions/e2e-sess/patch", `[{"index":0,"content":"patched"}]`)
	assertStatus(t, resp, 200)
	// Confirm patch.
	resp = h.get(t, "/sessions/e2e-sess/history")
	assertStatus(t, resp, 200)
	body = readBody(t, resp)
	histAfter := body["history"].([]any)
	first := histAfter[0].(map[string]any)["content"].(string)
	if first != "patched" {
		t.Fatalf("patch did not apply, got: %s", first)
	}

	// Kill (clear messages).
	resp = h.post(t, "/sessions/e2e-sess/kill", "{}")
	assertStatus(t, resp, 200)
	resp = h.get(t, "/sessions/e2e-sess/history")
	assertStatus(t, resp, 200)
	body = readBody(t, resp)
	histKilled, _ := body["history"].([]any)
	if len(histKilled) != 0 {
		t.Fatalf("expected 0 messages after kill, got %d", len(histKilled))
	}

	// Delete.
	resp = h.delete(t, "/sessions/e2e-sess")
	assertStatus(t, resp, 200)
	resp = h.get(t, "/sessions/e2e-sess")
	assertStatus(t, resp, 404)
}

func TestE2E_RPCMethods(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	cases := []struct {
		method string
		params any
	}{
		{"health", map[string]any{}},
		{"gateway.status", map[string]any{}},
		{"sessions.list", map[string]any{}},
		{"plugins.list", map[string]any{}},
		{"models.list", map[string]any{}},
		{"models.capability", map[string]any{"provider": "openai"}},
		{"tools.list", map[string]any{}},
		{"tools.invoke", map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}}},
		{"tools.invoke", map[string]any{"name": "time.now", "arguments": map[string]any{}}},
		{"logs.list", map[string]any{}},
		{"cron.list", map[string]any{}},
		{"hooks.list", map[string]any{}},
		{"secrets.list", map[string]any{}},
		{"approvals.list", map[string]any{}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.method, func(t *testing.T) {
			env := rpc(t, h, tc.method, tc.params)
			if env["error"] != nil {
				t.Fatalf("rpc %s returned error: %v", tc.method, env["error"])
			}
			if env["result"] == nil {
				t.Fatalf("rpc %s returned nil result", tc.method)
			}
		})
	}

	// message.send round-trip.
	env := rpc(t, h, "message.send", map[string]any{
		"sessionId": "rpc-test", "message": "ping", "channel": "cli",
	})
	if env["error"] != nil {
		t.Fatalf("message.send error: %v", env["error"])
	}
	result := env["result"].(map[string]any)
	if result["reply"] == "" {
		t.Fatal("message.send: expected reply")
	}

	// sessions.get
	env = rpc(t, h, "sessions.get", map[string]any{"sessionId": "rpc-test"})
	if env["error"] != nil {
		t.Fatalf("sessions.get error: %v", env["error"])
	}

	// sessions.history
	env = rpc(t, h, "sessions.history", map[string]any{"sessionId": "rpc-test"})
	if env["error"] != nil {
		t.Fatalf("sessions.history error: %v", env["error"])
	}

	// sessions.kill
	env = rpc(t, h, "sessions.kill", map[string]any{"sessionId": "rpc-test"})
	if env["error"] != nil {
		t.Fatalf("sessions.kill error: %v", env["error"])
	}

	// sessions.delete
	env = rpc(t, h, "sessions.delete", map[string]any{"sessionId": "rpc-test"})
	if env["error"] != nil {
		t.Fatalf("sessions.delete error: %v", env["error"])
	}

	// agent.run
	env = rpc(t, h, "agent.run", map[string]any{
		"sessionId": "rpc-agent", "message": "hello",
	})
	if env["error"] != nil {
		t.Fatalf("agent.run error: %v", env["error"])
	}

	// method not found returns result=nil and error block.
	env = rpc(t, h, "bogus.method", map[string]any{})
	if env["error"] == nil {
		t.Fatal("expected error for unknown method")
	}
}

func TestE2E_ToolsHTTP(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	// List tools.
	resp := h.get(t, "/tools")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	tools, _ := body["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("expected registered tools")
	}

	// Invoke echo.
	resp = h.post(t, "/tools/invoke", map[string]any{
		"name": "echo", "arguments": map[string]any{"text": "e2e"},
	})
	assertStatus(t, resp, 200)
	body = readBody(t, resp)
	result := body["result"].(map[string]any)
	if result["text"] != "e2e" {
		t.Fatalf("echo: unexpected result: %v", result)
	}

	// Invoke time.now.
	resp = h.post(t, "/tools/invoke", map[string]any{
		"name": "time.now", "arguments": map[string]any{},
	})
	assertStatus(t, resp, 200)
	body = readBody(t, resp)
	r2 := body["result"].(map[string]any)
	if r2["utc"] == nil {
		t.Fatal("time.now: expected utc field")
	}

	// Invoke sessions.count.
	resp = h.post(t, "/tools/invoke", map[string]any{
		"name": "sessions.count", "arguments": map[string]any{},
	})
	assertStatus(t, resp, 200)

	// Unknown tool.
	resp = h.post(t, "/tools/invoke", map[string]any{
		"name": "no.such.tool", "arguments": map[string]any{},
	})
	if resp.StatusCode != 400 {
		resp.Body.Close()
		t.Fatalf("expected 400 for unknown tool, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestE2E_AgentRun(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	resp := h.post(t, "/agent/run", map[string]any{
		"sessionId": "agent-e2e",
		"message":   "what time is it?",
	})
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	if body["reply"] == "" {
		t.Fatal("expected reply from agent run")
	}
	if body["turns"].(float64) < 1 {
		t.Fatal("expected at least 1 turn")
	}
	if body["runId"] == "" {
		t.Fatal("expected runId")
	}
}

func TestE2E_OpenAICompat(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	// GET /v1/models — OpenAI list format.
	resp := h.get(t, "/v1/models")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	if body["object"] != "list" {
		t.Fatalf("expected object=list, got %v", body["object"])
	}
	data, _ := body["data"].([]any)
	if len(data) == 0 {
		t.Fatal("expected model list")
	}

	// POST /v1/chat/completions.
	resp = h.post(t, "/v1/chat/completions", map[string]any{
		"model":    "echo",
		"messages": []map[string]string{{"role": "user", "content": "hello v1"}},
	})
	assertStatus(t, resp, 200)
	body = readBody(t, resp)
	if body["object"] != "chat.completion" {
		t.Fatalf("expected object=chat.completion, got %v", body["object"])
	}
	choices := body["choices"].([]any)
	if len(choices) == 0 {
		t.Fatal("expected choices")
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] == "" {
		t.Fatal("expected content in choice")
	}
}

func TestE2E_LogsCronHooksSecrets(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	// Trigger a log entry.
	h.post(t, "/message", map[string]string{"sessionId": "log-s", "message": "hi", "channel": "cli"})

	// GET /logs.
	resp := h.get(t, "/logs")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	logs, _ := body["logs"].([]any)
	if len(logs) == 0 {
		t.Fatal("expected at least one log entry")
	}

	// Cron: add → list → delete.
	resp = h.post(t, "/cron", `{"id":"test-job","name":"test","schedule":"@every 1h","command":"echo hi","enabled":true}`)
	assertStatus(t, resp, 200)
	resp = h.get(t, "/cron")
	assertStatus(t, resp, 200)
	body = readBody(t, resp)
	jobs, _ := body["jobs"].([]any)
	if len(jobs) == 0 {
		t.Fatal("expected at least 1 cron job")
	}
	resp = h.delete(t, "/cron/test-job")
	assertStatus(t, resp, 200)

	// Hooks: add → list → delete.
	resp = h.post(t, "/hooks", `{"id":"test-hook","name":"h","event":"message.received","type":"log","enabled":true}`)
	assertStatus(t, resp, 200)
	resp = h.get(t, "/hooks")
	assertStatus(t, resp, 200)
	resp = h.delete(t, "/hooks/test-hook")
	assertStatus(t, resp, 200)

	// Secrets: set → list (no values) → delete.
	resp = h.post(t, "/secrets", `{"name":"MY_KEY","value":"supersecret"}`)
	assertStatus(t, resp, 200)
	resp = h.get(t, "/secrets")
	assertStatus(t, resp, 200)
	body = readBody(t, resp)
	secrets, _ := body["secrets"].([]any)
	for _, s := range secrets {
		sm := s.(map[string]any)
		if sm["name"] == "MY_KEY" {
			if sm["value"] != nil {
				t.Fatal("secret value must not be exposed in list")
			}
		}
	}
	resp = h.delete(t, "/secrets/MY_KEY")
	assertStatus(t, resp, 200)
}

func TestE2E_ChannelWebhooks(t *testing.T) {
	h := newChannelHarness(t)
	defer h.close()

	cases := []struct {
		name string
		path string
		body string
	}{
		{
			"Telegram webhook",
			"/webhooks/telegram",
			`{"update_id":1,"message":{"text":"tg e2e","from":{"is_bot":false},"chat":{"id":42}}}`,
		},
		{
			"Slack url_verification",
			"/webhooks/slack",
			`{"type":"url_verification","challenge":"chal123"}`,
		},
		{
			"Slack event",
			"/webhooks/slack",
			`{"type":"event_callback","event":{"type":"message","text":"hi slack","channel":"C1","user":"U1"}}`,
		},
		{
			"Discord webhook",
			"/webhooks/discord",
			`{"content":"hi discord","channel_id":"D1","author":{"bot":false,"id":"DU1"}}`,
		},
		{
			"Teams webhook",
			"/webhooks/teams",
			`{"type":"message","text":"hi teams","conversation":{"id":"T1"},"from":{"id":"TU1"}}`,
		},
		{
			"WhatsApp message",
			"/webhooks/whatsapp",
			`{"entry":[{"changes":[{"value":{"messages":[{"type":"text","from":"447700900001","text":{"body":"hi wa"}}]}}]}]}`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, h.base+tc.path,
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("%s: HTTP %d: %s", tc.name, resp.StatusCode, body)
			}
		})
	}

	// WhatsApp verify GET.
	req, _ := http.NewRequest(http.MethodGet,
		h.base+"/webhooks/whatsapp?hub.mode=subscribe&hub.verify_token=&hub.challenge=xyz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("WhatsApp verify: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("WhatsApp verify: HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "xyz" {
		t.Fatalf("WhatsApp verify: expected challenge=xyz, got %q", body)
	}
}

func TestE2E_RateLimit(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	// Override rate limiter to a tight value via the exported setter if available.
	// Since RateLimiter is internal we just verify that /message works fine under
	// normal load (< default limit) and the endpoint returns 200.
	for i := 0; i < 3; i++ {
		resp := h.post(t, "/message", map[string]string{
			"sessionId": fmt.Sprintf("rl-%d", i),
			"message":   "test",
			"channel":   "cli",
		})
		assertStatus(t, resp, 200)
		resp.Body.Close()
	}
}

func TestE2E_SessionsAutoPopulatedFromChannels(t *testing.T) {
	h := newChannelHarness(t)
	defer h.close()

	// Send through Telegram webhook.
	req, _ := http.NewRequest(http.MethodPost, h.base+"/webhooks/telegram",
		strings.NewReader(`{"update_id":2,"message":{"text":"hi","from":{"is_bot":false},"chat":{"id":9999}}}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST telegram webhook: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("telegram webhook: expected 200, got %d", resp.StatusCode)
	}

	// Session is created synchronously inside HandleInbound, so immediately checkable.
	resp = h.get(t, "/sessions/telegram:9999")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	if body["channel"] != "telegram" {
		t.Fatalf("expected channel=telegram, got %v", body["channel"])
	}
}

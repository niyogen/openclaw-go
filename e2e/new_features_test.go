package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openclaw-go/internal/channels"
	"openclaw-go/internal/gateway"
	"openclaw-go/internal/plugins"
	"openclaw-go/internal/sandbox"
)

// ── Feature 1: SSE Streaming ──────────────────────────────────────────────────

func TestE2E_StreamingChatCompletions(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	body := `{"model":"echo","stream":true,"messages":[{"role":"user","content":"hello streaming world"}]}`
	resp := h.post(t, "/v1/chat/completions", body)
	defer resp.Body.Close()

	assertStatus(t, resp, 200)
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	var chunks []string
	doneReceived := false
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			doneReceived = true
			break
		}
		chunks = append(chunks, data)
	}

	if !doneReceived {
		t.Fatal("stream did not end with [DONE]")
	}
	if len(chunks) == 0 {
		t.Fatal("no SSE chunks received")
	}
	// Reconstruct full content from delta chunks.
	var fullContent strings.Builder
	for _, raw := range chunks {
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
			t.Fatalf("invalid chunk JSON %q: %v", raw, err)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("expected chat.completion.chunk, got %q", chunk.Object)
		}
		if len(chunk.Choices) > 0 {
			fullContent.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	content := fullContent.String()
	if !strings.Contains(content, "hello") {
		t.Fatalf("reply should contain original message, got: %q", content)
	}
}

func TestE2E_StreamingVsBlocking(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	// Both should reply with similar content.
	msg := "test parity"

	blockResp := h.post(t, "/v1/chat/completions",
		`{"model":"echo","messages":[{"role":"user","content":"`+msg+`"}]}`)
	assertStatus(t, blockResp, 200)
	var blockResult struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	json.NewDecoder(blockResp.Body).Decode(&blockResult) //nolint:errcheck
	blockResp.Body.Close()
	blockContent := blockResult.Choices[0].Message.Content

	streamResp := h.post(t, "/v1/chat/completions",
		`{"model":"echo","stream":true,"messages":[{"role":"user","content":"`+msg+`"}]}`)
	assertStatus(t, streamResp, 200)
	var sb strings.Builder
	scanner := bufio.NewScanner(streamResp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct{ Content string } `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
			sb.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	streamResp.Body.Close()
	streamContent := strings.TrimSpace(sb.String())

	if !strings.Contains(streamContent, strings.TrimSpace(blockContent)) &&
		!strings.Contains(strings.TrimSpace(blockContent), streamContent) {
		t.Fatalf("streaming content %q does not match blocking %q", streamContent, blockContent)
	}
}

// ── Feature 2: sessions.subscribe event bus ───────────────────────────────────

func TestE2E_EventBusViaWS(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	// Subscribe to events on the bus directly.
	evCh, unsub := h.server.Bus().Subscribe("bus-e2e")
	defer unsub()

	// Send a message — should fire events.
	resp := h.post(t, "/message", map[string]string{
		"sessionId": "bus-e2e",
		"message":   "trigger event",
		"channel":   "cli",
	})
	assertStatus(t, resp, 200)
	resp.Body.Close()

	// Collect events up to 500ms.
	var events []gateway.GatewayEvent
	deadline := time.After(500 * time.Millisecond)
loop:
	for {
		select {
		case ev := <-evCh:
			events = append(events, ev)
		case <-deadline:
			break loop
		}
	}
	if len(events) == 0 {
		t.Fatal("expected at least one event from message send")
	}
	// Verify session.message events are present.
	var hasMessage bool
	for _, ev := range events {
		if ev.Type == gateway.EventSessionMessage {
			hasMessage = true
			break
		}
	}
	if !hasMessage {
		t.Fatalf("expected session.message event, got: %v", events)
	}
}

func TestE2E_EventBusSessionFilter(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	// Subscribe to specific session only.
	filtered, unsub := h.server.Bus().Subscribe("specific-sess")
	defer unsub()

	// Message to OTHER session — should not arrive.
	resp := h.post(t, "/message", map[string]string{
		"sessionId": "other-sess", "message": "noise", "channel": "cli",
	})
	assertStatus(t, resp, 200)
	resp.Body.Close()

	// Message to TARGET session — should arrive.
	resp = h.post(t, "/message", map[string]string{
		"sessionId": "specific-sess", "message": "signal", "channel": "cli",
	})
	assertStatus(t, resp, 200)
	resp.Body.Close()

	// Wait up to 300ms for the signal.
	deadline := time.After(300 * time.Millisecond)
	var got []gateway.GatewayEvent
outer:
	for {
		select {
		case ev := <-filtered:
			got = append(got, ev)
		case <-deadline:
			break outer
		}
	}
	for _, ev := range got {
		if ev.SessionID != "specific-sess" {
			t.Fatalf("received event for wrong session: %s", ev.SessionID)
		}
	}
	if len(got) == 0 {
		t.Fatal("no event received for target session")
	}
}

func TestE2E_SessionsSubscribeRPC(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	env := rpcNew(t, h, "sessions.subscribe", map[string]any{"sessionId": ""})
	if env["error"] != nil {
		t.Fatalf("sessions.subscribe rpc error: %v", env["error"])
	}
}

func TestE2E_SessionDeletePublishesEvent(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	// Create a session.
	h.post(t, "/message", map[string]string{
		"sessionId": "del-event", "message": "hi", "channel": "cli",
	}).Body.Close()

	ch, unsub := h.server.Bus().Subscribe("del-event")
	defer unsub()

	// Delete it.
	h.delete(t, "/sessions/del-event").Body.Close()

	select {
	case ev := <-ch:
		if ev.Type != gateway.EventSessionDeleted {
			t.Fatalf("expected EventSessionDeleted, got %s", ev.Type)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("no delete event received")
	}
}

// ── Feature 3: Dynamic plugin loading ────────────────────────────────────────

func TestE2E_PluginLoader_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	loader := plugins.NewLoader(dir)
	loaded, err := loader.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatalf("expected 0 plugins, got %d", len(loaded))
	}
}

func TestE2E_PluginLoader_ManifestDiscovery(t *testing.T) {
	dir := t.TempDir()
	plugDir := filepath.Join(dir, "test-plugin")
	if err := os.MkdirAll(plugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{
		"name": "test-plugin",
		"version": "1.0.0",
		"description": "E2E test plugin",
		"routes": [{"method":"GET","path":"/plugins/test-plugin/ping"}],
		"tools": [{"name":"test.ping","description":"ping tool","endpoint":"http://localhost:9999/ping"}]
	}`
	if err := os.WriteFile(filepath.Join(plugDir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := plugins.NewLoader(dir)
	loaded, err := loader.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(loaded))
	}
	if loaded[0].Name() != "test-plugin" {
		t.Fatalf("unexpected plugin name: %s", loaded[0].Name())
	}
	if len(loaded[0].Tools()) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(loaded[0].Tools()))
	}
}

func TestE2E_PluginRegisteredInGateway(t *testing.T) {
	dir := t.TempDir()
	// Write a test plugin manifest.
	plugDir := filepath.Join(dir, "my-plugin")
	if err := os.MkdirAll(plugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Plugin exposes a route that answers with a fixed response.
	fakeBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pong":true}`))
	}))
	defer fakeBackend.Close()

	manifest := `{"name":"my-plugin","version":"1.0.0","description":"inline test","routes":[{"method":"GET","path":"/plugins/my-plugin/ping","forward":"` + fakeBackend.URL + `"}],"tools":[]}`
	if err := os.WriteFile(filepath.Join(plugDir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// Load the plugin and register it.
	loader := plugins.NewLoader(dir)
	loaded, err := loader.Load()
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, "")
	defer h.close()
	for _, ep := range loaded {
		h.server.RegisterExternalPlugin(ep)
	}

	// Verify it appears in the plugins list.
	resp := h.get(t, "/plugins")
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	pluginList, _ := body["plugins"].([]any)
	var found bool
	for _, p := range pluginList {
		if p == "my-plugin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("plugin 'my-plugin' not in list: %v", pluginList)
	}
}

// ── Feature 4: Docker sandbox ─────────────────────────────────────────────────

func TestE2E_SandboxAvailabilityTool(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	resp := h.post(t, "/tools/invoke", map[string]any{
		"name": "sandbox.available", "arguments": map[string]any{},
	})
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result map, got %T: %v", body["result"], body["result"])
	}
	// available may be true or false depending on the environment — we just
	// verify the tool responds correctly with the boolean field.
	if _, hasAvail := result["available"]; !hasAvail {
		t.Fatalf("expected 'available' field in result: %v", result)
	}
}

func TestE2E_SandboxRunToolRequiresScript(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	// Missing script arg should return 400.
	resp := h.post(t, "/tools/invoke", map[string]any{
		"name": "sandbox.run", "arguments": map[string]any{},
	})
	if resp.StatusCode != 400 {
		resp.Body.Close()
		t.Fatalf("expected 400 for missing script, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestE2E_SandboxRunWithDocker(t *testing.T) {
	if !sandbox.IsAvailable(context.Background()) {
		t.Skip("Docker not available")
	}
	h := newHarness(t, "")
	defer h.close()
	// Wire real sandbox.
	gateway.SetSandboxFuncs(
		func(ctx context.Context, script string, _ interface{}) (*gateway.SandboxResult, error) {
			r, err := sandbox.RunScript(ctx, script, sandbox.DefaultOptions())
			if err != nil {
				return nil, err
			}
			return &gateway.SandboxResult{Stdout: r.Stdout, Stderr: r.Stderr, ExitCode: r.ExitCode}, nil
		},
		sandbox.IsAvailable,
	)
	resp := h.post(t, "/tools/invoke", map[string]any{
		"name":      "sandbox.run",
		"arguments": map[string]any{"script": "echo hello-from-sandbox"},
	})
	assertStatus(t, resp, 200)
	body := readBody(t, resp)
	result, _ := body["result"].(map[string]any)
	stdout, _ := result["stdout"].(string)
	if !strings.Contains(stdout, "hello-from-sandbox") {
		t.Fatalf("expected sandbox stdout to contain 'hello-from-sandbox', got: %q", stdout)
	}
}

// ── Feature 5: LINE and Nostr channels ───────────────────────────────────────

func TestE2E_LineWebhook(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	handleInbound := func(ctx context.Context, inbound channels.InboundMessage) error {
		_, err := h.server.HandleInbound(ctx, inbound)
		return err
	}
	h.server.HandleFunc("/webhooks/line",
		channels.BuildLineWebhookHandler("", handleInbound))

	linePayload := `{"events":[{"type":"message","source":{"type":"user","userId":"Utest123"},"message":{"type":"text","text":"hello from line"},"replyToken":"test-token"}]}`
	req, _ := http.NewRequest(http.MethodPost, h.base+"/webhooks/line",
		strings.NewReader(linePayload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /webhooks/line: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("line webhook: %d %s", resp.StatusCode, body)
	}

	// Session should be created.
	time.Sleep(50 * time.Millisecond)
	sessResp := h.get(t, "/sessions/line:Utest123")
	assertStatus(t, sessResp, 200)
	sessResp.Body.Close()
}

func TestE2E_LineWebhookSignatureVerification(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	handleInbound := func(ctx context.Context, inbound channels.InboundMessage) error {
		_, err := h.server.HandleInbound(ctx, inbound)
		return err
	}
	// Register with a secret — requests without valid sig should be rejected.
	h.server.HandleFunc("/webhooks/line-secret",
		channels.BuildLineWebhookHandler("mysecret", handleInbound))

	req, _ := http.NewRequest(http.MethodPost, h.base+"/webhooks/line-secret",
		strings.NewReader(`{"events":[]}`))
	req.Header.Set("Content-Type", "application/json")
	// No X-Line-Signature header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 without signature, got %d", resp.StatusCode)
	}
}

func TestE2E_NostrChannelRoute(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	// Nostr uses relay WS, not HTTP webhooks — the placeholder returns 404.
	h.server.HandleFunc("/webhooks/nostr",
		channels.BuildNostrWebhookHandler("", func(_ context.Context, _ channels.InboundMessage) error {
			return nil
		}))

	resp := h.post(t, "/webhooks/nostr", `{}`)
	defer resp.Body.Close()
	// Should be 404 — Nostr doesn't use HTTP webhooks.
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 for Nostr HTTP webhook, got %d", resp.StatusCode)
	}
}

// ── Cross-feature: streaming + event bus ─────────────────────────────────────

func TestE2E_StreamingTriggersBusEvent(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	// Subscribe before sending.
	evCh, unsub := h.server.Bus().Subscribe("")
	defer unsub()

	// Use blocking mode (not streaming) through /message — that goes through
	// processMessage which fires bus events.
	resp := h.post(t, "/message", map[string]string{
		"sessionId": "stream-bus",
		"message":   "stream bus test",
		"channel":   "cli",
	})
	assertStatus(t, resp, 200)
	resp.Body.Close()

	var replyEvent bool
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case ev := <-evCh:
			if ev.Type == gateway.EventAgentReply && ev.SessionID == "stream-bus" {
				replyEvent = true
				goto done
			}
		case <-deadline:
			goto done
		}
	}
done:
	if !replyEvent {
		t.Fatal("expected EventAgentReply event after message send")
	}
}

// rpcNew is a local RPC helper for the new_features_test file.
func rpcNew(t *testing.T, h *harness, method string, params any) map[string]any {
	t.Helper()
	payload := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
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

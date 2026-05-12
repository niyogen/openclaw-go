package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"openclaw-go/internal/channels"
	"openclaw-go/internal/plugins"
)

// writeChannelPluginManifest writes a plugin.json under root/name/ that
// declares a channel plugin pointing at baseURL. Helper for the tests
// below — production manifests look identical.
func writeChannelPluginManifest(t *testing.T, root, name, channel, baseURL string) {
	t.Helper()
	pluginDir := filepath.Join(root, name)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := plugins.Manifest{
		Name:    name,
		Version: "0.1.0",
		Channel: &plugins.ChannelManifest{Channel: channel, BaseURL: baseURL},
	}
	raw, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubChannelPlugin is an httptest server pretending to be a channel
// plugin. Captures outbound dispatches the gateway sends so tests can
// assert what flowed through.
type stubChannelPlugin struct {
	server   *httptest.Server
	mu       sync.Mutex
	received []channels.OutboundMessage
}

func newStubChannelPlugin(t *testing.T) *stubChannelPlugin {
	t.Helper()
	sp := &stubChannelPlugin{}
	mux := http.NewServeMux()
	mux.HandleFunc("/channel/send", func(w http.ResponseWriter, r *http.Request) {
		var msg channels.OutboundMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		sp.mu.Lock()
		sp.received = append(sp.received, msg)
		sp.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	sp.server = httptest.NewServer(mux)
	t.Cleanup(sp.server.Close)
	return sp
}

func (s *stubChannelPlugin) URL() string { return s.server.URL }
func (s *stubChannelPlugin) sent() []channels.OutboundMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]channels.OutboundMessage, len(s.received))
	copy(out, s.received)
	return out
}

// channelPluginTestSetup builds: a Server, a temp manifest dir, the
// channel-plugin registry pointing at it, a stub plugin process, and
// returns everything wired up like main.go would.
func channelPluginTestSetup(t *testing.T) (*Server, *plugins.ChannelPluginRegistry, *stubChannelPlugin) {
	t.Helper()
	s := buildTestServer(t, "")

	stub := newStubChannelPlugin(t)
	pluginsDir := t.TempDir()
	writeChannelPluginManifest(t, pluginsDir, "demo-plugin", "demo", stub.URL())

	reg, err := plugins.NewChannelPluginRegistry(pluginsDir, filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetChannelPluginRegistry(reg)

	// Mount the inbound handler (main.go does this; replicate here so the
	// test exercises the same path).
	s.mux.HandleFunc("/plugins/{name}/inbound",
		plugins.BuildChannelPluginInboundHandler(reg, func(ctx context.Context, m channels.InboundMessage) error {
			_, err := s.HandleInbound(ctx, m)
			return err
		}))

	return s, reg, stub
}

// ──────────────────────────────────────────────────────────────────────
// plugins.channel.* RPCs
// ──────────────────────────────────────────────────────────────────────

func TestRPCPluginsChannelList(t *testing.T) {
	s, _, _ := channelPluginTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "plugins.channel.list", "params": map[string]any{}}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/rpc", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Plugins []map[string]any `json:"plugins"`
		} `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Result.Plugins) != 1 {
		t.Fatalf("expected 1 channel plugin, got %d", len(out.Result.Plugins))
	}
	if out.Result.Plugins[0]["state"] != "pending" {
		t.Fatalf("state should be pending until approve; got %v", out.Result.Plugins[0])
	}
}

func TestRPCPluginsChannelApproveThenList(t *testing.T) {
	s, _, _ := channelPluginTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	// Approve.
	approveBody := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "plugins.channel.approve", "params": map[string]any{"name": "demo-plugin"}}
	raw, _ := json.Marshal(approveBody)
	resp, err := http.Post(ts.URL+"/rpc", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var approveOut struct {
		Result struct {
			Token string `json:"token"`
			State string `json:"state"`
		} `json:"result"`
	}
	_ = json.Unmarshal(body, &approveOut)
	if approveOut.Result.Token == "" || approveOut.Result.State != "approved" {
		t.Fatalf("approve response: %s", body)
	}

	// List — state should flip.
	listBody := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "plugins.channel.list", "params": map[string]any{}}
	raw, _ = json.Marshal(listBody)
	resp2, err := http.Post(ts.URL+"/rpc", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var listOut struct {
		Result struct {
			Plugins []map[string]any `json:"plugins"`
		} `json:"result"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&listOut)
	if listOut.Result.Plugins[0]["state"] != "approved" {
		t.Fatalf("state should be approved after approve; got %v", listOut.Result.Plugins[0])
	}
}

func TestRPCPluginsChannelRevoke(t *testing.T) {
	s, reg, _ := channelPluginTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	if _, err := reg.Approve("demo-plugin"); err != nil {
		t.Fatal(err)
	}

	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "plugins.channel.revoke", "params": map[string]any{"name": "demo-plugin"}}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/rpc", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if reg.List()[0].State != "pending" {
		t.Fatal("plugin should be back to pending after revoke")
	}
}

// ──────────────────────────────────────────────────────────────────────
// End-to-end: approved plugin → router dispatch → stub plugin receives
// + plugin → inbound handler → session store
// ──────────────────────────────────────────────────────────────────────

func TestChannelPluginEndToEndDispatch(t *testing.T) {
	s, reg, stub := channelPluginTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	// Approve and register the plugin's channel with the router.
	tok, err := reg.Approve("demo-plugin")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range reg.ApprovedManifests() {
		s.route.Register(plugins.NewPluginChannel(m))
	}

	// Trigger a message.send RPC. This drives the full agent loop —
	// `message.send` accepts a USER message, runs the EchoRunner, and
	// dispatches the ASSISTANT's REPLY to the channel. So the plugin
	// receives the echo of the input, not the input itself. That's the
	// production behavior; testing it confirms the whole pipeline
	// (inbound → runner → outbound) flows through the plugin.
	rpcBody := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "message.send",
		"params": map[string]any{
			"sessionId": "e2e-1",
			"channel":   "demo",
			"target":    "user-1",
			"message":   "hello-via-plugin",
		},
	}
	raw, _ := json.Marshal(rpcBody)
	resp, err := http.Post(ts.URL+"/rpc", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Allow router dispatch + http roundtrip to settle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(stub.sent()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(stub.sent()) == 0 {
		t.Fatalf("stub plugin did not receive the outbound message")
	}
	got := stub.sent()[0]
	if got.Target != "user-1" || got.SessionID != "e2e-1" {
		t.Errorf("plugin received wrong target/session: %+v", got)
	}
	// EchoRunner replies with the original message echoed back; assert
	// that the input is present in the assistant reply.
	if !bytes.Contains([]byte(got.Message), []byte("hello-via-plugin")) {
		t.Errorf("plugin reply should contain echoed input; got %q", got.Message)
	}

	// Now exercise the inbound direction: plugin POSTs to /plugins/demo-plugin/inbound
	// with its token. Gateway should accept + create a session.
	inboundBody := map[string]any{
		"sessionId": "e2e-inbound",
		"channel":   "ignored", // gateway overwrites from manifest
		"target":    "user-1",
		"message":   "inbound from plugin",
	}
	raw, _ = json.Marshal(inboundBody)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/plugins/demo-plugin/inbound", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("inbound: %d %s", resp2.StatusCode, body)
	}

	// Session should be persisted with the channel name from the manifest, not "ignored".
	sess, ok := s.store.Get("e2e-inbound")
	if !ok {
		t.Fatal("inbound session not created")
	}
	if sess.Channel != "demo" {
		t.Errorf("session channel: got %q want %q (manifest-authoritative)", sess.Channel, "demo")
	}
}

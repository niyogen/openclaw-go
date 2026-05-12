package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openclaw-go/internal/hookstore"
	"openclaw-go/internal/plugins"
)

// writeHookPluginManifest writes a plugin.json under root/name/ that
// declares a hooks[] array.
func writeHookPluginManifest(t *testing.T, root, name string, hooks []plugins.ManifestHook) {
	t.Helper()
	pluginDir := filepath.Join(root, name)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := plugins.Manifest{
		Name:    name,
		Version: "0.1.0",
		Hooks:   hooks,
	}
	raw, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubHookPluginGateway is a httptest server that captures the
// envelope POSTs from the gateway-side dispatcher.
type stubHookPluginGateway struct {
	server   *httptest.Server
	mu       sync.Mutex
	received []map[string]any
}

func newStubHookPluginGateway(t *testing.T) *stubHookPluginGateway {
	t.Helper()
	sp := &stubHookPluginGateway{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var env map[string]any
		_ = json.NewDecoder(r.Body).Decode(&env)
		sp.mu.Lock()
		sp.received = append(sp.received, env)
		sp.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	sp.server = httptest.NewServer(mux)
	t.Cleanup(sp.server.Close)
	return sp
}

func (s *stubHookPluginGateway) URL() string { return s.server.URL }
func (s *stubHookPluginGateway) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.received)
}
func (s *stubHookPluginGateway) sent() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, len(s.received))
	copy(out, s.received)
	return out
}

func hookPluginTestSetup(t *testing.T) (*Server, *plugins.HookPluginRegistry, *stubHookPluginGateway) {
	t.Helper()
	s := buildTestServer(t, "")

	stub := newStubHookPluginGateway(t)
	pluginsDir := t.TempDir()
	writeHookPluginManifest(t, pluginsDir, "audit", []plugins.ManifestHook{
		{Event: "agent.run.complete", Endpoint: stub.URL() + "/hook/agent"},
	})

	reg, err := plugins.NewHookPluginRegistry(pluginsDir, filepath.Join(t.TempDir(), "hook-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetHookPluginRegistry(reg)
	return s, reg, stub
}

// ──────────────────────────────────────────────────────────────────────
// plugins.hook.* RPCs
// ──────────────────────────────────────────────────────────────────────

func TestRPCPluginsHookList(t *testing.T) {
	s, _, _ := hookPluginTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "plugins.hook.list", "params": map[string]any{}}
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
		t.Fatalf("expected 1 hook plugin, got %d", len(out.Result.Plugins))
	}
	if out.Result.Plugins[0]["state"] != "pending" {
		t.Errorf("state should be pending; got %v", out.Result.Plugins[0])
	}
}

func TestRPCPluginsHookApproveThenList(t *testing.T) {
	s, _, _ := hookPluginTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	approveBody := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "plugins.hook.approve", "params": map[string]any{"name": "audit"}}
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

	listBody := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "plugins.hook.list", "params": map[string]any{}}
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
		t.Errorf("state should be approved after approve; got %v", listOut.Result.Plugins[0])
	}
}

func TestRPCPluginsHookRevoke(t *testing.T) {
	s, reg, _ := hookPluginTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	if _, err := reg.Approve("audit"); err != nil {
		t.Fatal(err)
	}

	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "plugins.hook.revoke", "params": map[string]any{"name": "audit"}}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/rpc", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if reg.List()[0].State != "pending" {
		t.Fatal("should be back to pending after revoke")
	}
}

func TestRPCPluginsHookApproveErrorsOnUnknown(t *testing.T) {
	s, _, _ := hookPluginTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "plugins.hook.approve", "params": map[string]any{"name": "nope"}}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/rpc", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw2, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw2), "error") {
		t.Errorf("expected error response for unknown plugin; got %s", raw2)
	}
}

// ──────────────────────────────────────────────────────────────────────
// End-to-end: install dispatcher as a hookstore listener (matching what
// cmd/openclaw does at startup), fire Emit via the gateway's flow, and
// observe the envelope arriving at the stub plugin.
// ──────────────────────────────────────────────────────────────────────

func TestHookPluginEndToEndEmit(t *testing.T) {
	s, reg, stub := hookPluginTestSetup(t)
	if _, err := reg.Approve("audit"); err != nil {
		t.Fatal(err)
	}
	// Mirror main.go: install the dispatcher as a hookstore listener.
	s.HookStore().AddListener(plugins.NewPluginHookDispatcher(reg.ApprovedManifests()))

	// Fire via the same path the gateway uses.
	s.HookStore().Emit(hookstore.EventAgentRunComplete, map[string]any{"sessionId": "s1", "tokens": 42})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if stub.count() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if stub.count() != 1 {
		t.Fatalf("envelope not delivered; received=%d", stub.count())
	}
	got := stub.sent()[0]
	if got["event"] != "agent.run.complete" {
		t.Errorf("envelope event: %v", got["event"])
	}
	payload, _ := got["payload"].(map[string]any)
	if payload["sessionId"] != "s1" {
		t.Errorf("envelope payload: %+v", payload)
	}
	if got["timestamp"] == nil || got["timestamp"] == "" {
		t.Errorf("envelope timestamp missing")
	}
}

func TestHookPluginListenerSurvivesNoMatch(t *testing.T) {
	// Listener installed but the fired event doesn't match any plugin
	// subscription — Emit must complete without errors and without
	// reaching the plugin endpoint.
	s, reg, stub := hookPluginTestSetup(t)
	if _, err := reg.Approve("audit"); err != nil {
		t.Fatal(err)
	}
	s.HookStore().AddListener(plugins.NewPluginHookDispatcher(reg.ApprovedManifests()))

	called := atomic.Int32{}
	s.HookStore().AddListener(func(event hookstore.EventType, payload map[string]any) {
		called.Add(1)
	})

	// Emit an event the plugin did NOT subscribe to.
	s.HookStore().Emit(hookstore.EventMessageReceived, map[string]any{})
	time.Sleep(100 * time.Millisecond)
	if stub.count() != 0 {
		t.Errorf("non-matching event should not reach plugin; count=%d", stub.count())
	}
	if called.Load() != 1 {
		t.Errorf("control listener should still fire on non-matching event; got %d", called.Load())
	}
}

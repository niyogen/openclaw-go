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
	"strings"
	"sync"
	"testing"

	"openclaw-go/internal/plugins"
)

// writeToolPluginManifest writes a plugin.json under root/name/ that
// declares a tools[] array. Helper for the tests below.
func writeToolPluginManifest(t *testing.T, root, name string, tools []plugins.ManifestTool) {
	t.Helper()
	pluginDir := filepath.Join(root, name)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := plugins.Manifest{
		Name:    name,
		Version: "0.1.0",
		Tools:   tools,
	}
	raw, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubToolPlugin is an httptest server pretending to be a tool plugin.
// Captures every POST so tests can assert what the gateway forwarded.
type stubToolPlugin struct {
	server   *httptest.Server
	mu       sync.Mutex
	received []map[string]any
	respond  func(http.ResponseWriter)
}

func newStubToolPlugin(t *testing.T) *stubToolPlugin {
	t.Helper()
	sp := &stubToolPlugin{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var args map[string]any
		_ = json.NewDecoder(r.Body).Decode(&args)
		sp.mu.Lock()
		sp.received = append(sp.received, args)
		respond := sp.respond
		sp.mu.Unlock()
		if respond != nil {
			respond(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"sunny in Colombo"}`))
	})
	sp.server = httptest.NewServer(mux)
	t.Cleanup(sp.server.Close)
	return sp
}

func (s *stubToolPlugin) URL() string { return s.server.URL }
func (s *stubToolPlugin) sent() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, len(s.received))
	copy(out, s.received)
	return out
}

// toolPluginTestSetup mirrors channelPluginTestSetup: builds a Server,
// a temp manifest dir, the tool-plugin registry pointing at it, a stub
// plugin, and wires the registry onto the Server. Does NOT auto-register
// tools (the test does that after approving) since the production flow
// registers tools at startup using `ApprovedManifests`.
func toolPluginTestSetup(t *testing.T) (*Server, *plugins.ToolPluginRegistry, *stubToolPlugin) {
	t.Helper()
	s := buildTestServer(t, "")

	stub := newStubToolPlugin(t)
	pluginsDir := t.TempDir()
	writeToolPluginManifest(t, pluginsDir, "weather-plugin", []plugins.ManifestTool{
		{Name: "weather", Description: "look up weather", Endpoint: stub.URL() + "/tool/weather"},
	})

	reg, err := plugins.NewToolPluginRegistry(pluginsDir, filepath.Join(t.TempDir(), "tool-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetToolPluginRegistry(reg)
	return s, reg, stub
}

// ──────────────────────────────────────────────────────────────────────
// plugins.tool.* RPCs
// ──────────────────────────────────────────────────────────────────────

func TestRPCPluginsToolList(t *testing.T) {
	s, _, _ := toolPluginTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "plugins.tool.list", "params": map[string]any{}}
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
		t.Fatalf("expected 1 tool plugin, got %d", len(out.Result.Plugins))
	}
	if out.Result.Plugins[0]["state"] != "pending" {
		t.Fatalf("state should be pending until approve; got %v", out.Result.Plugins[0])
	}
}

func TestRPCPluginsToolApproveThenList(t *testing.T) {
	s, _, _ := toolPluginTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	approveBody := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "plugins.tool.approve", "params": map[string]any{"name": "weather-plugin"}}
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

	listBody := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "plugins.tool.list", "params": map[string]any{}}
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

func TestRPCPluginsToolRevoke(t *testing.T) {
	s, reg, _ := toolPluginTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	if _, err := reg.Approve("weather-plugin"); err != nil {
		t.Fatal(err)
	}

	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "plugins.tool.revoke", "params": map[string]any{"name": "weather-plugin"}}
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

func TestRPCPluginsToolApproveErrorsOnUnknown(t *testing.T) {
	s, _, _ := toolPluginTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "plugins.tool.approve", "params": map[string]any{"name": "nope"}}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/rpc", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw2, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw2), "error") {
		t.Fatalf("expected error response for unknown plugin; got %s", raw2)
	}
}

// ──────────────────────────────────────────────────────────────────────
// End-to-end: approved tool plugin registered with the gateway's tool
// registry → `tools.invoke` → plugin endpoint receives args → response
// flows back through `tools.invoke`.
// ──────────────────────────────────────────────────────────────────────

func TestToolPluginEndToEndInvoke(t *testing.T) {
	s, reg, stub := toolPluginTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	// Approve and register each declared tool with the gateway tool
	// registry, matching what cmd/openclaw/main.go does at startup.
	if _, err := reg.Approve("weather-plugin"); err != nil {
		t.Fatal(err)
	}
	for _, m := range reg.ApprovedManifests() {
		for _, mt := range m.Tools {
			tname := mt.Name
			h := plugins.NewPluginToolHandler(mt.Endpoint)
			s.tools.Register(
				Tool{Name: tname, Description: mt.Description},
				func(ctx context.Context, args map[string]any) (any, error) {
					return h(ctx, args)
				},
			)
		}
	}

	// Drive `tools.invoke` via /rpc.
	invokeBody := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools.invoke",
		"params": map[string]any{
			"name":      "weather",
			"arguments": map[string]any{"city": "Colombo"},
		},
	}
	raw, _ := json.Marshal(invokeBody)
	resp, err := http.Post(ts.URL+"/rpc", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result map[string]any `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Result["result"] != "sunny in Colombo" {
		t.Fatalf("tools.invoke result: %+v", out.Result)
	}

	// Verify the plugin actually received the args.
	sent := stub.sent()
	if len(sent) != 1 || sent[0]["city"] != "Colombo" {
		t.Errorf("plugin received: %+v", sent)
	}
}

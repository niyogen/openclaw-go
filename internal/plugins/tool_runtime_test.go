package plugins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// stubToolPlugin is an httptest-backed stand-in for an external tool
// plugin. Captures every POST so the test can assert what the gateway
// would forward.
type stubToolPlugin struct {
	mu       sync.Mutex
	server   *httptest.Server
	received []map[string]any
	respond  func(http.ResponseWriter) // optional override
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
		_, _ = w.Write([]byte(`{"result":"ok"}`))
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

// ──────────────────────────────────────────────────────────────────────
// Manifest scanning + approval lifecycle
// ──────────────────────────────────────────────────────────────────────

func TestToolRegistryListsToolPluginsAsPending(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "weather-plugin", Manifest{
		Name:    "weather-plugin",
		Version: "0.1.0",
		Tools: []ManifestTool{
			{Name: "weather", Description: "look up weather", Endpoint: "https://example.com/tool/weather"},
		},
	})

	reg, err := NewToolPluginRegistry(root, filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	entries := reg.List()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].State != "pending" {
		t.Errorf("state: %q (want pending)", entries[0].State)
	}
	if len(entries[0].Tools) != 1 || entries[0].Tools[0].Name != "weather" {
		t.Errorf("tools: %+v", entries[0].Tools)
	}
}

func TestToolRegistryIgnoresManifestsWithoutTools(t *testing.T) {
	root := t.TempDir()
	// Channel-only manifest — should be skipped by the tool registry.
	writeManifest(t, root, "channel-only", Manifest{
		Name:    "channel-only",
		Channel: &ChannelManifest{Channel: "demo", BaseURL: "http://127.0.0.1:9999"},
	})
	reg, err := NewToolPluginRegistry(root, filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.List()) != 0 {
		t.Errorf("channel-only manifest should not appear in tool registry")
	}
}

func TestToolRegistryAllowsLoopbackEndpoints(t *testing.T) {
	// Tool plugins run as sidecars on loopback by design (same as channel
	// plugins). The scan layer must NOT reject loopback URLs — that's
	// what approval-gating is for. The legacy Loader.Load() flow does
	// SSRF-validate, but the tool-plugin registry pin's its security
	// boundary at approval time instead.
	root := t.TempDir()
	writeManifest(t, root, "sidecar", Manifest{
		Name: "sidecar",
		Tools: []ManifestTool{
			{Name: "ping", Endpoint: "http://127.0.0.1:9201/tool/ping"},
		},
	})
	reg, err := NewToolPluginRegistry(root, filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.List()) != 1 {
		t.Fatalf("loopback endpoint manifest should be visible; got %+v", reg.List())
	}
}

func TestToolRegistryApproveIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "demo", Manifest{
		Name:  "demo",
		Tools: []ManifestTool{{Name: "ping", Endpoint: "https://example.com/ping"}},
	})
	reg, err := NewToolPluginRegistry(root, filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	tok1, err := reg.Approve("demo")
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := reg.Approve("demo")
	if err != nil {
		t.Fatal(err)
	}
	if tok1 == "" || tok1 != tok2 {
		t.Errorf("approve should be idempotent; got %q vs %q", tok1, tok2)
	}
}

func TestToolRegistryRevokeReturnsPlaceholderToPending(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "demo", Manifest{
		Name:  "demo",
		Tools: []ManifestTool{{Name: "ping", Endpoint: "https://example.com/ping"}},
	})
	reg, err := NewToolPluginRegistry(root, filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Approve("demo"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Revoke("demo"); err != nil {
		t.Fatal(err)
	}
	entries := reg.List()
	if len(entries) != 1 || entries[0].State != "pending" {
		t.Errorf("after revoke, state should be pending; got %+v", entries)
	}
	// Revoking again is a no-op.
	if err := reg.Revoke("demo"); err != nil {
		t.Errorf("idempotent revoke errored: %v", err)
	}
}

func TestToolRegistryTokensPersistAcrossReopen(t *testing.T) {
	root := t.TempDir()
	tokensFile := filepath.Join(t.TempDir(), "tokens.json")
	writeManifest(t, root, "demo", Manifest{
		Name:  "demo",
		Tools: []ManifestTool{{Name: "ping", Endpoint: "https://example.com/ping"}},
	})
	reg1, err := NewToolPluginRegistry(root, tokensFile)
	if err != nil {
		t.Fatal(err)
	}
	want, err := reg1.Approve("demo")
	if err != nil {
		t.Fatal(err)
	}

	// Reopen — token should still be there.
	reg2, err := NewToolPluginRegistry(root, tokensFile)
	if err != nil {
		t.Fatal(err)
	}
	entries := reg2.List()
	if len(entries) != 1 || entries[0].State != "approved" {
		t.Errorf("expected one approved entry after reopen; got %+v", entries)
	}
	got, err := reg2.Approve("demo") // idempotent — should return same token
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("token rotated across reopen: got %q want %q", got, want)
	}
}

func TestToolRegistryApprovedManifestsReturnsOnlyApproved(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "approved-one", Manifest{
		Name:  "approved-one",
		Tools: []ManifestTool{{Name: "a", Endpoint: "https://example.com/a"}},
	})
	writeManifest(t, root, "pending-one", Manifest{
		Name:  "pending-one",
		Tools: []ManifestTool{{Name: "b", Endpoint: "https://example.com/b"}},
	})
	reg, err := NewToolPluginRegistry(root, filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Approve("approved-one"); err != nil {
		t.Fatal(err)
	}
	approved := reg.ApprovedManifests()
	if len(approved) != 1 || approved[0].Name != "approved-one" {
		t.Errorf("ApprovedManifests = %+v", approved)
	}
}

func TestToolRegistryApproveUnknownPluginFails(t *testing.T) {
	reg, err := NewToolPluginRegistry(t.TempDir(), filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Approve("nope"); err == nil {
		t.Error("approving unknown plugin should error")
	}
}

// ──────────────────────────────────────────────────────────────────────
// PluginToolHandler — gateway-side closure that POSTs to the plugin
// ──────────────────────────────────────────────────────────────────────

func TestPluginToolHandlerForwardsArgs(t *testing.T) {
	sp := newStubToolPlugin(t)
	h := NewPluginToolHandler(sp.URL() + "/tool/weather")
	out, err := h(context.Background(), map[string]any{"city": "Colombo"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "ok" {
		t.Errorf("result: %v (want ok)", out)
	}
	sent := sp.sent()
	if len(sent) != 1 || sent[0]["city"] != "Colombo" {
		t.Errorf("plugin received: %+v", sent)
	}
}

func TestPluginToolHandlerSurfacesEnvelopeError(t *testing.T) {
	sp := newStubToolPlugin(t)
	sp.respond = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"plugin said no"}`))
	}
	h := NewPluginToolHandler(sp.URL() + "/tool/x")
	_, err := h(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "plugin said no") {
		t.Errorf("envelope error not surfaced: %v", err)
	}
}

func TestPluginToolHandlerSurfaces5xx(t *testing.T) {
	sp := newStubToolPlugin(t)
	sp.respond = func(w http.ResponseWriter) {
		http.Error(w, "exploded", http.StatusInternalServerError)
	}
	h := NewPluginToolHandler(sp.URL() + "/tool/x")
	_, err := h(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("5xx not surfaced: %v", err)
	}
}

func TestPluginToolHandlerSurfacesConnectError(t *testing.T) {
	// 127.0.0.1:1 — should be ECONNREFUSED on most systems and quickly
	// fail rather than hang.
	h := NewPluginToolHandler("http://127.0.0.1:1/tool/nope")
	_, err := h(context.Background(), map[string]any{"x": 1})
	if err == nil {
		t.Fatal("expected connect error")
	}
}

func TestPluginToolHandlerHandlesPlainTextResponse(t *testing.T) {
	sp := newStubToolPlugin(t)
	sp.respond = func(w http.ResponseWriter) {
		// Plain text, not JSON envelope.
		_, _ = w.Write([]byte("just a string"))
	}
	h := NewPluginToolHandler(sp.URL() + "/tool/x")
	out, err := h(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "just a string" {
		t.Errorf("plain-text passthrough: got %v", out)
	}
}

func TestPluginToolHandlerHandlesRawJSONResponse(t *testing.T) {
	sp := newStubToolPlugin(t)
	sp.respond = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		// JSON, but not an envelope — a raw list. Should pass through
		// as the result.
		_, _ = w.Write([]byte(`[1,2,3]`))
	}
	h := NewPluginToolHandler(sp.URL() + "/tool/x")
	out, err := h(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Raw JSON decodes to []any of float64s; the contract is just
	// "passed through verbatim", so verify shape.
	list, ok := out.([]any)
	if !ok || len(list) != 3 {
		t.Errorf("raw-json passthrough lost shape: %v", out)
	}
}

// ──────────────────────────────────────────────────────────────────────
// HasToolPlugin — the manifest predicate
// ──────────────────────────────────────────────────────────────────────

func TestHasToolPluginPredicate(t *testing.T) {
	cases := []struct {
		name string
		m    Manifest
		want bool
	}{
		{"no tools", Manifest{}, false},
		{"empty entry", Manifest{Tools: []ManifestTool{{Name: "", Endpoint: ""}}}, false},
		{"only name", Manifest{Tools: []ManifestTool{{Name: "x", Endpoint: ""}}}, false},
		{"only endpoint", Manifest{Tools: []ManifestTool{{Name: "", Endpoint: "https://x"}}}, false},
		{"valid", Manifest{Tools: []ManifestTool{{Name: "x", Endpoint: "https://x"}}}, true},
		{"mixed", Manifest{Tools: []ManifestTool{{Name: "", Endpoint: ""}, {Name: "x", Endpoint: "https://x"}}}, true},
	}
	for _, tc := range cases {
		if got := tc.m.HasToolPlugin(); got != tc.want {
			t.Errorf("%s: HasToolPlugin = %v, want %v", tc.name, got, tc.want)
		}
	}
}

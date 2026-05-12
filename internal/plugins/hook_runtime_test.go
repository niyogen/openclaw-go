package plugins

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openclaw-go/internal/hookstore"
)

// stubHookPlugin captures every POST to /hook/<path>. Tests assert
// what envelope the gateway forwards.
type stubHookPlugin struct {
	server   *httptest.Server
	mu       sync.Mutex
	received []hookEnvelope
	respond  func(http.ResponseWriter)
}

func newStubHookPlugin(t *testing.T) *stubHookPlugin {
	t.Helper()
	sp := &stubHookPlugin{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var env hookEnvelope
		_ = json.NewDecoder(r.Body).Decode(&env)
		sp.mu.Lock()
		sp.received = append(sp.received, env)
		respond := sp.respond
		sp.mu.Unlock()
		if respond != nil {
			respond(w)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	sp.server = httptest.NewServer(mux)
	t.Cleanup(sp.server.Close)
	return sp
}

func (s *stubHookPlugin) URL() string { return s.server.URL }
func (s *stubHookPlugin) sent() []hookEnvelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]hookEnvelope, len(s.received))
	copy(out, s.received)
	return out
}

// ──────────────────────────────────────────────────────────────────────
// Manifest scanning + approval lifecycle
// ──────────────────────────────────────────────────────────────────────

func TestHookRegistryListsHookPluginsAsPending(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "audit", Manifest{
		Name: "audit",
		Hooks: []ManifestHook{
			{Event: "agent.run.complete", Endpoint: "http://127.0.0.1:9301/hook/agent"},
		},
	})
	reg, err := NewHookPluginRegistry(root, filepath.Join(t.TempDir(), "tokens.json"))
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
	if len(entries[0].Hooks) != 1 || entries[0].Hooks[0].Event != "agent.run.complete" {
		t.Errorf("hooks: %+v", entries[0].Hooks)
	}
}

func TestHookRegistryIgnoresManifestsWithoutHooks(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "tool-only", Manifest{
		Name:  "tool-only",
		Tools: []ManifestTool{{Name: "x", Endpoint: "https://example.com/x"}},
	})
	reg, err := NewHookPluginRegistry(root, filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.List()) != 0 {
		t.Errorf("tool-only manifest should not appear in hook registry")
	}
}

func TestHookRegistryApproveRevokeLifecycle(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "audit", Manifest{
		Name:  "audit",
		Hooks: []ManifestHook{{Event: "agent.run.complete", Endpoint: "http://x/hook"}},
	})
	reg, err := NewHookPluginRegistry(root, filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	tok1, err := reg.Approve("audit")
	if err != nil {
		t.Fatal(err)
	}
	tok2, _ := reg.Approve("audit")
	if tok1 != tok2 || tok1 == "" {
		t.Errorf("approve should be idempotent; got %q vs %q", tok1, tok2)
	}
	if err := reg.Revoke("audit"); err != nil {
		t.Fatal(err)
	}
	if reg.List()[0].State != "pending" {
		t.Errorf("revoke should return to pending")
	}
}

func TestHookRegistryTokensPersistAcrossReopen(t *testing.T) {
	root := t.TempDir()
	tokensFile := filepath.Join(t.TempDir(), "tokens.json")
	writeManifest(t, root, "audit", Manifest{
		Name:  "audit",
		Hooks: []ManifestHook{{Event: "agent.run.complete", Endpoint: "http://x/hook"}},
	})
	reg1, err := NewHookPluginRegistry(root, tokensFile)
	if err != nil {
		t.Fatal(err)
	}
	want, err := reg1.Approve("audit")
	if err != nil {
		t.Fatal(err)
	}
	reg2, err := NewHookPluginRegistry(root, tokensFile)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := reg2.Approve("audit")
	if got != want {
		t.Errorf("token rotated across reopen: got %q want %q", got, want)
	}
}

func TestHookRegistryApproveUnknownFails(t *testing.T) {
	reg, err := NewHookPluginRegistry(t.TempDir(), filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Approve("nope"); err == nil {
		t.Error("approving unknown plugin should error")
	}
}

// ──────────────────────────────────────────────────────────────────────
// NewPluginHookDispatcher + EventListener fan-out
// ──────────────────────────────────────────────────────────────────────

func TestDispatcherFiresMatchingEventOnly(t *testing.T) {
	sp := newStubHookPlugin(t)
	approved := []Manifest{
		{
			Name: "audit",
			Hooks: []ManifestHook{
				{Event: "agent.run.complete", Endpoint: sp.URL() + "/hook/agent"},
			},
		},
	}
	dispatcher := NewPluginHookDispatcher(approved)

	// Wrong event — should NOT fire.
	dispatcher(hookstore.EventMessageReceived, map[string]any{"x": 1})
	time.Sleep(50 * time.Millisecond)
	if len(sp.sent()) != 0 {
		t.Errorf("non-matching event should not fire; got %+v", sp.sent())
	}

	// Matching event — should fire and arrive within a few hundred ms.
	dispatcher(hookstore.EventAgentRunComplete, map[string]any{"sessionId": "s1", "ms": 1234})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sp.sent()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := sp.sent()
	if len(got) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(got))
	}
	if got[0].Event != "agent.run.complete" {
		t.Errorf("envelope event: %q", got[0].Event)
	}
	if got[0].Payload["sessionId"] != "s1" {
		t.Errorf("envelope payload: %+v", got[0].Payload)
	}
	if got[0].Timestamp == "" {
		t.Errorf("envelope timestamp empty")
	}
}

func TestDispatcherFansOutToMultipleEndpoints(t *testing.T) {
	sp1 := newStubHookPlugin(t)
	sp2 := newStubHookPlugin(t)
	approved := []Manifest{
		{Name: "a", Hooks: []ManifestHook{{Event: "agent.run.complete", Endpoint: sp1.URL() + "/h"}}},
		{Name: "b", Hooks: []ManifestHook{{Event: "agent.run.complete", Endpoint: sp2.URL() + "/h"}}},
	}
	dispatcher := NewPluginHookDispatcher(approved)
	dispatcher(hookstore.EventAgentRunComplete, map[string]any{"id": "x"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sp1.sent()) > 0 && len(sp2.sent()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(sp1.sent()) != 1 || len(sp2.sent()) != 1 {
		t.Errorf("both endpoints should receive: sp1=%d sp2=%d", len(sp1.sent()), len(sp2.sent()))
	}
}

func TestDispatcherSwallowsPluginFailures(t *testing.T) {
	// Plugin returns 500 — dispatcher must NOT block or error the caller.
	sp := newStubHookPlugin(t)
	sp.respond = func(w http.ResponseWriter) {
		http.Error(w, "exploded", http.StatusInternalServerError)
	}
	approved := []Manifest{
		{Name: "x", Hooks: []ManifestHook{{Event: "agent.run.complete", Endpoint: sp.URL() + "/h"}}},
	}
	dispatcher := NewPluginHookDispatcher(approved)
	// Returns immediately; failure is logged + dropped.
	dispatcher(hookstore.EventAgentRunComplete, map[string]any{})
	// Sleep to ensure the goroutine ran and didn't hang.
	time.Sleep(100 * time.Millisecond)
}

func TestDispatcherIgnoresUnreachableEndpoint(t *testing.T) {
	approved := []Manifest{
		{Name: "x", Hooks: []ManifestHook{{Event: "agent.run.complete", Endpoint: "http://127.0.0.1:1/hook"}}},
	}
	dispatcher := NewPluginHookDispatcher(approved)
	// Should not block or panic on connect-refused.
	dispatcher(hookstore.EventAgentRunComplete, map[string]any{})
	time.Sleep(100 * time.Millisecond)
}

// ──────────────────────────────────────────────────────────────────────
// HasHookPlugin predicate
// ──────────────────────────────────────────────────────────────────────

func TestHasHookPluginPredicate(t *testing.T) {
	cases := []struct {
		name string
		m    Manifest
		want bool
	}{
		{"no hooks", Manifest{}, false},
		{"empty entry", Manifest{Hooks: []ManifestHook{{Event: "", Endpoint: ""}}}, false},
		{"only event", Manifest{Hooks: []ManifestHook{{Event: "e", Endpoint: ""}}}, false},
		{"only endpoint", Manifest{Hooks: []ManifestHook{{Event: "", Endpoint: "https://x"}}}, false},
		{"valid", Manifest{Hooks: []ManifestHook{{Event: "e", Endpoint: "https://x"}}}, true},
	}
	for _, tc := range cases {
		if got := tc.m.HasHookPlugin(); got != tc.want {
			t.Errorf("%s: HasHookPlugin = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────
// End-to-end: install dispatcher as a hookstore listener, Emit, observe
// ──────────────────────────────────────────────────────────────────────

func TestDispatcherIntegratesWithHookstoreListener(t *testing.T) {
	sp := newStubHookPlugin(t)
	hitCount := atomic.Int32{}
	sp.respond = func(w http.ResponseWriter) {
		hitCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}

	hs, err := hookstore.New(filepath.Join(t.TempDir(), "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	approved := []Manifest{
		{Name: "x", Hooks: []ManifestHook{{Event: "agent.run.complete", Endpoint: sp.URL() + "/h"}}},
	}
	hs.AddListener(NewPluginHookDispatcher(approved))

	// Fire via hookstore.Emit — should reach the plugin.
	hs.Emit(hookstore.EventAgentRunComplete, map[string]any{"sessionId": "s1"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hitCount.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if hitCount.Load() != 1 {
		t.Errorf("hook delivered %d times, want 1", hitCount.Load())
	}
}

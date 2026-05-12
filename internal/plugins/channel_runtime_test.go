package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"openclaw-go/internal/channels"
)

// (writeManifest helper is shared with loader_test.go in the same package.)

// stubPlugin is an httptest-backed channel-plugin stand-in. It captures
// every /channel/send POST so the test can assert what the gateway would
// dispatch. Used in place of a real plugin binary.
type stubPlugin struct {
	mu       sync.Mutex
	server   *httptest.Server
	received []channels.OutboundMessage
	respond  func(http.ResponseWriter) // optional override
}

func newStubPlugin(t *testing.T) *stubPlugin {
	t.Helper()
	sp := &stubPlugin{}
	mux := http.NewServeMux()
	mux.HandleFunc("/channel/send", func(w http.ResponseWriter, r *http.Request) {
		body, _ := http.MaxBytesReader(w, r.Body, 1<<20), r.Body
		_ = body
		var msg channels.OutboundMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		sp.mu.Lock()
		sp.received = append(sp.received, msg)
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

func (s *stubPlugin) URL() string { return s.server.URL }

func (s *stubPlugin) sent() []channels.OutboundMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]channels.OutboundMessage, len(s.received))
	copy(out, s.received)
	return out
}

// ──────────────────────────────────────────────────────────────────────
// Manifest scanning + approval lifecycle
// ──────────────────────────────────────────────────────────────────────

func TestRegistryListsChannelPluginsAsPending(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "tele-style", Manifest{
		Name:    "tele-style",
		Version: "0.1.0",
		Channel: &ChannelManifest{
			Channel: "tele-style",
			BaseURL: "http://127.0.0.1:9101",
		},
	})

	reg, err := NewChannelPluginRegistry(root, filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("got %d entries, want 1", len(list))
	}
	if list[0].State != "pending" {
		t.Errorf("new plugin should be pending; got %q", list[0].State)
	}
	if list[0].Channel != "tele-style" {
		t.Errorf("channel name: %q", list[0].Channel)
	}
}

func TestApproveGeneratesTokenAndPersists(t *testing.T) {
	root := t.TempDir()
	tokens := filepath.Join(t.TempDir(), "tokens.json")
	writeManifest(t, root, "p1", Manifest{
		Channel: &ChannelManifest{Channel: "p1", BaseURL: "http://127.0.0.1:9101"},
	})

	reg, err := NewChannelPluginRegistry(root, tokens)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := reg.Approve("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 { // 32 bytes hex-encoded
		t.Fatalf("token length: got %d want 64", len(tok))
	}

	// Reload — the token must survive a registry restart.
	reg2, err := NewChannelPluginRegistry(root, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if reg2.tokenForPlugin("p1") != tok {
		t.Fatal("token did not persist across registry reload")
	}
	list := reg2.List()
	if list[0].State != "approved" {
		t.Errorf("after approve+reload, state should be approved; got %q", list[0].State)
	}
}

func TestApproveIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "p1", Manifest{
		Channel: &ChannelManifest{Channel: "p1", BaseURL: "http://x"},
	})
	reg, _ := NewChannelPluginRegistry(root, filepath.Join(t.TempDir(), "t.json"))
	tok1, _ := reg.Approve("p1")
	tok2, _ := reg.Approve("p1")
	if tok1 != tok2 {
		t.Fatal("repeat Approve should return same token, not rotate silently")
	}
}

func TestRevokeReturnsToPending(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "p1", Manifest{
		Channel: &ChannelManifest{Channel: "p1", BaseURL: "http://x"},
	})
	reg, _ := NewChannelPluginRegistry(root, filepath.Join(t.TempDir(), "t.json"))
	_, _ = reg.Approve("p1")
	if err := reg.Revoke("p1"); err != nil {
		t.Fatal(err)
	}
	list := reg.List()
	if len(list) != 1 || list[0].State != "pending" {
		t.Fatalf("after revoke, plugin must be pending; got %+v", list)
	}
}

func TestApproveUnknownPluginErrors(t *testing.T) {
	root := t.TempDir() // no manifest
	reg, _ := NewChannelPluginRegistry(root, filepath.Join(t.TempDir(), "t.json"))
	if _, err := reg.Approve("nonexistent"); err == nil {
		t.Fatal("approve of unknown plugin should error")
	}
}

func TestScanIgnoresNonChannelPlugins(t *testing.T) {
	// Existing route-only / tool-only plugins still load via Loader; the
	// channel registry must NOT pick them up.
	root := t.TempDir()
	writeManifest(t, root, "route-only", Manifest{
		Routes: []ManifestRoute{{Method: "GET", Path: "/x"}},
	})
	reg, _ := NewChannelPluginRegistry(root, filepath.Join(t.TempDir(), "t.json"))
	if list := reg.List(); len(list) != 0 {
		t.Fatalf("route-only plugin must not appear in channel registry; got %+v", list)
	}
}

// ──────────────────────────────────────────────────────────────────────
// pluginChannel.Send: outbound dispatch
// ──────────────────────────────────────────────────────────────────────

func TestPluginChannelSendsToStub(t *testing.T) {
	stub := newStubPlugin(t)
	ch := NewPluginChannel(Manifest{
		Channel: &ChannelManifest{Channel: "test", BaseURL: stub.URL()},
	})
	err := ch.Send(context.Background(), channels.OutboundMessage{
		SessionID: "s1", Target: "u1", Message: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	sent := stub.sent()
	if len(sent) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sent))
	}
	if sent[0].Message != "hello" || sent[0].Target != "u1" {
		t.Fatalf("plugin received wrong payload: %+v", sent[0])
	}
}

func TestPluginChannelSurfaces5xxAsError(t *testing.T) {
	stub := newStubPlugin(t)
	stub.respond = func(w http.ResponseWriter) {
		http.Error(w, "plugin overload", http.StatusInternalServerError)
	}
	ch := NewPluginChannel(Manifest{
		Channel: &ChannelManifest{Channel: "test", BaseURL: stub.URL()},
	})
	err := ch.Send(context.Background(), channels.OutboundMessage{Target: "u", Message: "x"})
	if err == nil {
		t.Fatal("expected error from 500 response")
	}
}

func TestPluginChannelDisabledWhenBaseURLEmpty(t *testing.T) {
	ch := NewPluginChannel(Manifest{Channel: &ChannelManifest{Channel: "c", BaseURL: ""}})
	// Disabled channel mirrors the built-in pattern: returns nil for send.
	if err := ch.Send(context.Background(), channels.OutboundMessage{Target: "u", Message: "x"}); err != nil {
		t.Fatalf("disabled channel should not error; got %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────
// Inbound handler: token auth + dispatch
// ──────────────────────────────────────────────────────────────────────

func TestInboundHandlerRejectsUnauthorized(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "p1", Manifest{
		Channel: &ChannelManifest{Channel: "p1", BaseURL: "http://x"},
	})
	reg, _ := NewChannelPluginRegistry(root, filepath.Join(t.TempDir(), "t.json"))
	_, _ = reg.Approve("p1")

	dispatchCalled := false
	h := BuildChannelPluginInboundHandler(reg, func(_ context.Context, _ channels.InboundMessage) error {
		dispatchCalled = true
		return nil
	})

	// Missing token.
	req := httptest.NewRequest(http.MethodPost, "/plugins/p1/inbound",
		bytes.NewBufferString(`{"sessionId":"s","channel":"p1","message":"hi"}`))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d want 401", rec.Code)
	}

	// Wrong token.
	req = httptest.NewRequest(http.MethodPost, "/plugins/p1/inbound",
		bytes.NewBufferString(`{"sessionId":"s","channel":"p1","message":"hi"}`))
	req.Header.Set("Authorization", "Bearer bogus-token")
	rec = httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d want 401", rec.Code)
	}

	if dispatchCalled {
		t.Fatal("dispatch should not run when auth fails")
	}
}

func TestInboundHandlerDispatchesOnGoodAuth(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "p1", Manifest{
		Channel: &ChannelManifest{Channel: "p1", BaseURL: "http://x"},
	})
	reg, _ := NewChannelPluginRegistry(root, filepath.Join(t.TempDir(), "t.json"))
	tok, _ := reg.Approve("p1")

	var seen channels.InboundMessage
	h := BuildChannelPluginInboundHandler(reg, func(_ context.Context, m channels.InboundMessage) error {
		seen = m
		return nil
	})

	req := httptest.NewRequest(http.MethodPost, "/plugins/p1/inbound",
		bytes.NewBufferString(`{"sessionId":"s1","channel":"PLUGIN-SET","target":"u1","message":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth ok: got %d want 200", rec.Code)
	}
	if seen.SessionID != "s1" || seen.Message != "hello" {
		t.Fatalf("dispatched message wrong: %+v", seen)
	}
	// Gateway must overwrite the channel name from the manifest, not
	// trust the plugin-supplied body — prevents impersonation.
	if seen.Channel != "p1" {
		t.Errorf("channel should be gateway-authoritative; got %q", seen.Channel)
	}
}

func TestInboundHandlerRejectsUnknownPlugin(t *testing.T) {
	reg, _ := NewChannelPluginRegistry(t.TempDir(), filepath.Join(t.TempDir(), "t.json"))
	h := BuildChannelPluginInboundHandler(reg, func(_ context.Context, _ channels.InboundMessage) error { return nil })

	req := httptest.NewRequest(http.MethodPost, "/plugins/never-existed/inbound",
		bytes.NewBufferString(`{}`))
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin: got %d want 404", rec.Code)
	}
}

func TestInboundHandlerRejectsPendingPlugin(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "p1", Manifest{
		Channel: &ChannelManifest{Channel: "p1", BaseURL: "http://x"},
	})
	reg, _ := NewChannelPluginRegistry(root, filepath.Join(t.TempDir(), "t.json"))
	// NOT calling Approve — plugin stays pending.

	h := BuildChannelPluginInboundHandler(reg, func(_ context.Context, _ channels.InboundMessage) error { return nil })

	req := httptest.NewRequest(http.MethodPost, "/plugins/p1/inbound",
		bytes.NewBufferString(`{}`))
	req.Header.Set("Authorization", "Bearer whatever")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNotFound {
		// Pending plugins shouldn't reveal their existence — 404 not 401.
		t.Fatalf("pending plugin: got %d want 404 (must not leak pending state)", rec.Code)
	}
}

func TestInboundHandlerSurfacesDispatchError(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "p1", Manifest{
		Channel: &ChannelManifest{Channel: "p1", BaseURL: "http://x"},
	})
	reg, _ := NewChannelPluginRegistry(root, filepath.Join(t.TempDir(), "t.json"))
	tok, _ := reg.Approve("p1")

	h := BuildChannelPluginInboundHandler(reg, func(_ context.Context, _ channels.InboundMessage) error {
		return errStub
	})

	req := httptest.NewRequest(http.MethodPost, "/plugins/p1/inbound",
		bytes.NewBufferString(`{"sessionId":"s","channel":"p1","message":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("dispatch err: got %d want 500", rec.Code)
	}
}

var errStub = &simpleError{"dispatch failed"}

type simpleError struct{ s string }

func (e *simpleError) Error() string { return e.s }

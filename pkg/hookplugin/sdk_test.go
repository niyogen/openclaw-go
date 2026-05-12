package hookplugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadFromEnvHappyPath(t *testing.T) {
	t.Setenv("OPENCLAW_PLUGIN_NAME", "audit")
	t.Setenv("OPENCLAW_GATEWAY_URL", "http://127.0.0.1:18789")
	t.Setenv("OPENCLAW_PLUGIN_TOKEN", "tok-xyz")
	p, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "audit" || p.GatewayURL != "http://127.0.0.1:18789" || p.Token != "tok-xyz" {
		t.Errorf("loaded plugin: %+v", p)
	}
}

func TestLoadFromEnvMissingVarsReportsAll(t *testing.T) {
	t.Setenv("OPENCLAW_PLUGIN_NAME", "")
	t.Setenv("OPENCLAW_GATEWAY_URL", "")
	t.Setenv("OPENCLAW_PLUGIN_TOKEN", "")
	_, err := LoadFromEnv()
	if err == nil {
		t.Fatal("expected error when env vars missing")
	}
	for _, v := range []string{"OPENCLAW_PLUGIN_NAME", "OPENCLAW_GATEWAY_URL", "OPENCLAW_PLUGIN_TOKEN"} {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("missing-var error should mention %s; got %v", v, err)
		}
	}
}

func TestHandlePathNormalizesAndDispatches(t *testing.T) {
	p := &Plugin{}
	var seen Envelope
	// Register without leading slash — SDK should add it.
	p.HandlePath("hook/agent", func(ctx context.Context, env Envelope) {
		seen = env
	})
	if p.Paths()[0] != "/hook/agent" {
		t.Errorf("path should be normalized to leading slash; got %v", p.Paths())
	}

	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)

	body := `{"event":"agent.run.complete","payload":{"sessionId":"s1"},"timestamp":"2026-05-12T12:00:00Z"}`
	resp, err := http.Post(srv.URL+"/hook/agent", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if seen.Event != "agent.run.complete" || seen.Payload["sessionId"] != "s1" {
		t.Errorf("envelope: %+v", seen)
	}
}

func TestHandlePathSortedPaths(t *testing.T) {
	p := &Plugin{}
	p.HandlePath("/c", func(ctx context.Context, env Envelope) {})
	p.HandlePath("/a", func(ctx context.Context, env Envelope) {})
	p.HandlePath("/b", func(ctx context.Context, env Envelope) {})
	got := p.Paths()
	want := []string{"/a", "/b", "/c"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Paths()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHandlePathRejectsEmptyAndNil(t *testing.T) {
	p := &Plugin{}
	p.HandlePath("", func(ctx context.Context, env Envelope) {})
	p.HandlePath("  ", func(ctx context.Context, env Envelope) {})
	p.HandlePath("/x", nil)
	if len(p.Paths()) != 0 {
		t.Errorf("invalid registrations should be rejected; got %v", p.Paths())
	}
}

func TestHandlerLastWriteWins(t *testing.T) {
	p := &Plugin{}
	var v atomic.Int32
	p.HandlePath("/x", func(ctx context.Context, env Envelope) { v.Store(1) })
	p.HandlePath("/x", func(ctx context.Context, env Envelope) { v.Store(2) })
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/x", "application/json", strings.NewReader(`{"event":"e","payload":{},"timestamp":"t"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if v.Load() != 2 {
		t.Errorf("last-write-wins: stored %d, want 2", v.Load())
	}
}

func TestHandlerReturns404ForUnknownPath(t *testing.T) {
	p := &Plugin{}
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/nope", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: %d (want 404)", resp.StatusCode)
	}
}

func TestHandlerRejectsNonPOST(t *testing.T) {
	p := &Plugin{}
	p.HandlePath("/x", func(ctx context.Context, env Envelope) {})
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status: %d", resp.StatusCode)
	}
}

func TestHandlerRejectsInvalidEnvelope(t *testing.T) {
	p := &Plugin{}
	p.HandlePath("/x", func(ctx context.Context, env Envelope) {})
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/x", "application/json", strings.NewReader(`not-json`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: %d (want 400)", resp.StatusCode)
	}
}

func TestHandlerSurvivesHandlerPanic(t *testing.T) {
	p := &Plugin{}
	p.HandlePath("/boom", func(ctx context.Context, env Envelope) {
		panic("kaboom")
	})
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	body := `{"event":"e","payload":{},"timestamp":"t"}`
	resp, err := http.Post(srv.URL+"/boom", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status: %d (want 500)", resp.StatusCode)
	}
	// Server should still be alive — a second request to a different path succeeds.
	p.HandlePath("/ok", func(ctx context.Context, env Envelope) {})
	resp2, err := http.Post(srv.URL+"/ok", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("server should still be alive; got %d", resp2.StatusCode)
	}
}

func TestHandlerReceivesRequestContext(t *testing.T) {
	// Handler context comes from r.Context() — verifies it's non-nil
	// and tied to the request (cancels when the client disconnects).
	// We don't try to propagate client-side deadlines across the
	// network boundary — that's not how HTTP works. The contract is
	// just "handler receives a usable ctx".
	p := &Plugin{}
	gotCtx := make(chan context.Context, 1)
	p.HandlePath("/x", func(ctx context.Context, env Envelope) {
		gotCtx <- ctx
	})
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/x", "application/json",
		strings.NewReader(`{"event":"e","payload":{},"timestamp":"t"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case ctx := <-gotCtx:
		if ctx == nil {
			t.Error("handler received nil ctx")
		}
	case <-time.After(time.Second):
		t.Error("handler did not run")
	}
}

func TestEnvelopeJSONShape(t *testing.T) {
	// Pin the wire shape — the gateway produces this exact body, so the
	// SDK must decode it unchanged.
	env := Envelope{
		Event:     "agent.run.complete",
		Payload:   map[string]any{"sessionId": "s1", "tokens": 12},
		Timestamp: "2026-05-12T12:00:00Z",
	}
	raw, _ := json.Marshal(env)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	if decoded["event"] != "agent.run.complete" {
		t.Errorf("event field: %v", decoded["event"])
	}
	if decoded["timestamp"] != "2026-05-12T12:00:00Z" {
		t.Errorf("timestamp field: %v", decoded["timestamp"])
	}
	payload, _ := decoded["payload"].(map[string]any)
	if payload["sessionId"] != "s1" {
		t.Errorf("payload: %v", payload)
	}
}

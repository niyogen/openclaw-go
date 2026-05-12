package channelplugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoadFromEnvHappyPath(t *testing.T) {
	t.Setenv("OPENCLAW_PLUGIN_NAME", "test-plugin")
	t.Setenv("OPENCLAW_GATEWAY_URL", "http://127.0.0.1:18789")
	t.Setenv("OPENCLAW_PLUGIN_TOKEN", "tok-xyz")

	p, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "test-plugin" || p.GatewayURL != "http://127.0.0.1:18789" || p.Token != "tok-xyz" {
		t.Fatalf("plugin load mismatch: %+v", p)
	}
}

func TestLoadFromEnvReportsAllMissing(t *testing.T) {
	t.Setenv("OPENCLAW_PLUGIN_NAME", "")
	t.Setenv("OPENCLAW_GATEWAY_URL", "")
	t.Setenv("OPENCLAW_PLUGIN_TOKEN", "")

	_, err := LoadFromEnv()
	if err == nil {
		t.Fatal("expected error with no env vars set")
	}
	for _, want := range []string{"OPENCLAW_PLUGIN_NAME", "OPENCLAW_GATEWAY_URL", "OPENCLAW_PLUGIN_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention missing var %q; got: %v", want, err)
		}
	}
}

func TestHandlerInvokesOnSend(t *testing.T) {
	var seen OutboundMessage
	p := &Plugin{
		Name:       "test",
		GatewayURL: "http://x",
		Token:      "t",
		OnSend: func(_ context.Context, msg OutboundMessage) error {
			seen = msg
			return nil
		},
	}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	body := `{"sessionId":"s1","channel":"test","target":"u1","message":"hello"}`
	resp, err := http.Post(srv.URL+"/channel/send", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if seen.Message != "hello" || seen.Target != "u1" {
		t.Fatalf("OnSend got wrong payload: %+v", seen)
	}
}

func TestHandlerSurfacesOnSendError(t *testing.T) {
	p := &Plugin{
		Name:       "test",
		GatewayURL: "http://x",
		Token:      "t",
		OnSend: func(_ context.Context, _ OutboundMessage) error {
			return errors.New("downstream API is on fire")
		},
	}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/channel/send", "application/json",
		strings.NewReader(`{"message":"x","target":"y"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("OnSend error: got %d want 500", resp.StatusCode)
	}
}

func TestHandlerRejectsBadJSON(t *testing.T) {
	p := &Plugin{OnSend: func(_ context.Context, _ OutboundMessage) error { return nil }}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/channel/send", "application/json",
		strings.NewReader(`not json`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d want 400", resp.StatusCode)
	}
}

func TestHandlerRejectsMissingOnSend(t *testing.T) {
	p := &Plugin{} // OnSend nil
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/channel/send", "application/json",
		strings.NewReader(`{"message":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("got %d want 501", resp.StatusCode)
	}
}

func TestDispatchInboundPostsWithBearer(t *testing.T) {
	var seenAuth, seenPath, seenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenPath = r.URL.Path
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		seenBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &Plugin{
		Name:       "test-plugin",
		GatewayURL: srv.URL,
		Token:      "tok-abc",
	}
	err := p.DispatchInbound(context.Background(), InboundMessage{
		SessionID: "s1",
		Channel:   "test-plugin",
		Target:    "user@x",
		Message:   "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenAuth != "Bearer tok-abc" {
		t.Errorf("auth header: %q", seenAuth)
	}
	if seenPath != "/plugins/test-plugin/inbound" {
		t.Errorf("path: %q", seenPath)
	}
	var got InboundMessage
	_ = json.Unmarshal([]byte(seenBody), &got)
	if got.Message != "hi" || got.SessionID != "s1" {
		t.Errorf("body: %+v", got)
	}
}

func TestDispatchInboundSurfacesNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := &Plugin{Name: "p", GatewayURL: srv.URL, Token: "tok"}
	err := p.DispatchInbound(context.Background(), InboundMessage{Message: "hi"})
	if err == nil {
		t.Fatal("expected error from 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status code; got %v", err)
	}
}

func TestDispatchInboundRequiresConfig(t *testing.T) {
	p := &Plugin{} // no name/url/token
	if err := p.DispatchInbound(context.Background(), InboundMessage{Message: "x"}); err == nil {
		t.Fatal("expected error when plugin not configured")
	}
}

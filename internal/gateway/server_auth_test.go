package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"openclaw-go/internal/agents"
	"openclaw-go/internal/channels"
	"openclaw-go/internal/plugins"
	"openclaw-go/internal/sessions"
)

func buildTestServer(t *testing.T, authToken string) *Server {
	t.Helper()
	dir := t.TempDir()
	store, err := sessions.New(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("sessions.New failed: %v", err)
	}
	registry := plugins.NewRegistry()
	registry.Register(plugins.NewMetaPlugin(registry))
	return New(
		"127.0.0.1",
		0,
		authToken,
		[]string{"http://127.0.0.1"},
		store,
		&agents.EchoRunner{},
		channels.NewRouter(),
		registry,
		dir,
	)
}

func TestHealthRouteIsPublic(t *testing.T) {
	s := buildTestServer(t, "secret123")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
}

func TestSessionsRouteRequiresAuthWhenEnabled(t *testing.T) {
	s := buildTestServer(t, "secret123")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/sessions", nil)
	if err != nil {
		t.Fatalf("building request failed: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sessions failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", resp.StatusCode)
	}
}

func TestSessionsRouteAcceptsBearerToken(t *testing.T) {
	s := buildTestServer(t, "secret123")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/sessions", nil)
	if err != nil {
		t.Fatalf("building request failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sessions failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
}

package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openclaw-go/internal/agents"
	"openclaw-go/internal/channels"
	"openclaw-go/internal/plugins"
	"openclaw-go/internal/sessions"
)

// testDataDir creates a temp dir with retry-cleanup for Windows file-handle release.
func testDataDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gateway-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 5; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(time.Duration(i+1) * 50 * time.Millisecond)
		}
	})
	return dir
}

func buildTestServer(t *testing.T, authToken string) *Server {
	t.Helper()
	dir := testDataDir(t)
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

func TestTrustedProxyCIDR(t *testing.T) {
	s := buildTestServer(t, "secret")
	s.SetAuth("", []string{"10.0.0.0/8"})
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// A request claiming to come from 10.1.2.3 (inside the /8) should be allowed.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/sessions", nil)
	req.Header.Set("X-Forwarded-For", "10.1.2.3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for trusted CIDR, got %d", resp.StatusCode)
	}

	// A request from outside the CIDR should be denied.
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/sessions", nil)
	req2.Header.Set("X-Forwarded-For", "192.168.1.1")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for untrusted IP, got %d", resp2.StatusCode)
	}
}

func TestTrustedProxyLiteralIP(t *testing.T) {
	s := buildTestServer(t, "secret")
	s.SetAuth("", []string{"192.168.5.5"})
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/sessions", nil)
	req.Header.Set("X-Forwarded-For", "192.168.5.5")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for literal trusted IP, got %d", resp.StatusCode)
	}
}

func TestBodyLimitRejectsOversizedPayload(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// Build a payload larger than maxBodyBytes (4 MiB).
	huge := `{"sessionId":"x","message":"` + strings.Repeat("a", maxBodyBytes+1) + `"}`
	resp, err := http.Post(ts.URL+"/message", "application/json", bytes.NewBufferString(huge))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected non-200 for oversized body, got %d", resp.StatusCode)
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

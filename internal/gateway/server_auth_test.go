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
	s := New(
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
	s.SetAgentSummary("echo", "echo", false, false)
	return s
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

func TestMetricsRouteIsPublic(t *testing.T) {
	s := buildTestServer(t, "secret123")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("content-type: %q", ct)
	}
}

func TestMetricsRequireAuthRejectsWithoutCredentials(t *testing.T) {
	s := buildTestServer(t, "secret123")
	s.SetMetricsRequireAuth(true)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestMetricsRequireAuthAcceptsBearer(t *testing.T) {
	s := buildTestServer(t, "secret123")
	s.SetMetricsRequireAuth(true)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestMetricsRequireAuthNoEffectWithoutGatewayAuth(t *testing.T) {
	s := buildTestServer(t, "") // no token
	s.SetMetricsRequireAuth(true)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 when gateway auth disabled, got %d", resp.StatusCode)
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

// authReq builds a synthetic *http.Request and drives s.isAuthorized directly
// so we can exercise the trusted-proxy code path with a controllable
// RemoteAddr — httptest.NewServer always serves over loopback, which would
// only let us test the (spoofable) header-based path.
func authReq(remoteAddr string, headers map[string]string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://example/sessions", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestTrustedProxyCIDR(t *testing.T) {
	s := buildTestServer(t, "secret")
	s.SetAuth("", []string{"10.0.0.0/8"})

	// Direct peer inside the trusted CIDR is authorized without a token.
	if !s.isAuthorized(authReq("10.1.2.3:54321", nil)) {
		t.Fatalf("expected authorized for direct peer in trusted CIDR")
	}
	// Direct peer outside the CIDR is rejected.
	if s.isAuthorized(authReq("192.168.1.1:54321", nil)) {
		t.Fatalf("expected unauthorized for direct peer outside trusted CIDR")
	}
}

func TestTrustedProxyLiteralIP(t *testing.T) {
	s := buildTestServer(t, "secret")
	s.SetAuth("", []string{"192.168.5.5"})

	if !s.isAuthorized(authReq("192.168.5.5:443", nil)) {
		t.Fatalf("expected authorized for direct peer matching literal trusted IP")
	}
	if s.isAuthorized(authReq("192.168.5.6:443", nil)) {
		t.Fatalf("expected unauthorized for direct peer not in trusted list")
	}
}

// TestTrustedProxyXFFSpoofRejected guards against the previously-shipped
// vulnerability where clientIP() trusted X-Forwarded-For unconditionally,
// letting an attacker set XFF to a trusted-proxy IP and bypass auth. The
// trust check must use RemoteAddr (the actual TCP peer), not header content.
func TestTrustedProxyXFFSpoofRejected(t *testing.T) {
	s := buildTestServer(t, "secret")
	s.SetAuth("", []string{"10.0.0.0/8"})

	// Untrusted peer claims to be a trusted-CIDR IP via X-Forwarded-For.
	spoof := authReq("8.8.8.8:54321", map[string]string{"X-Forwarded-For": "10.1.2.3"})
	if s.isAuthorized(spoof) {
		t.Fatalf("XFF spoof of trusted CIDR must NOT bypass auth")
	}

	// Same attack via X-Real-IP.
	spoof2 := authReq("8.8.8.8:54321", map[string]string{"X-Real-IP": "10.1.2.3"})
	if s.isAuthorized(spoof2) {
		t.Fatalf("X-Real-IP spoof of trusted CIDR must NOT bypass auth")
	}

	// Same attack against literal trusted IP.
	s.SetAuth("", []string{"192.168.5.5"})
	spoof3 := authReq("203.0.113.1:54321", map[string]string{"X-Forwarded-For": "192.168.5.5"})
	if s.isAuthorized(spoof3) {
		t.Fatalf("XFF spoof of literal trusted IP must NOT bypass auth")
	}
}

// TestClientIPHonorsXFFOnlyFromTrustedPeer verifies clientIP's contract: XFF
// is honored only when the direct peer is itself in the trusted-proxy list.
func TestClientIPHonorsXFFOnlyFromTrustedPeer(t *testing.T) {
	trusted := []string{"10.0.0.0/8"}

	// Untrusted peer with XFF: returns the direct peer IP.
	got := clientIP(authReq("8.8.8.8:1234", map[string]string{"X-Forwarded-For": "1.2.3.4"}), trusted)
	if got != "8.8.8.8" {
		t.Fatalf("untrusted peer XFF leak: got %q, want 8.8.8.8", got)
	}
	// Trusted peer with XFF: returns the forwarded IP.
	got = clientIP(authReq("10.0.0.5:1234", map[string]string{"X-Forwarded-For": "1.2.3.4"}), trusted)
	if got != "1.2.3.4" {
		t.Fatalf("trusted peer XFF: got %q, want 1.2.3.4", got)
	}
	// Trusted peer, comma-separated XFF: returns the leftmost (real client) entry.
	got = clientIP(authReq("10.0.0.5:1234", map[string]string{"X-Forwarded-For": "1.2.3.4, 10.0.0.5"}), trusted)
	if got != "1.2.3.4" {
		t.Fatalf("trusted peer XFF chain: got %q, want 1.2.3.4", got)
	}
	// IPv6 peer (no trusted proxies configured): returns the host without brackets/port.
	got = clientIP(authReq("[::1]:443", nil), nil)
	if got != "::1" {
		t.Fatalf("IPv6 direct peer: got %q, want ::1", got)
	}
}

// TestSetAuthRaceFreeUnderRace exercises concurrent SetAuthToken and
// isAuthorized to make sure -race in CI catches any future regression in
// auth-field locking. The assertion is just non-panic + sane result; the
// race detector does the heavy lifting.
func TestSetAuthRaceFreeUnderRace(t *testing.T) {
	s := buildTestServer(t, "tok-a")

	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			if i%2 == 0 {
				s.SetAuthToken("tok-a")
			} else {
				s.SetAuthToken("tok-b")
			}
		}
		close(done)
	}()
	for i := 0; i < 500; i++ {
		r := authReq("127.0.0.1:9999", map[string]string{"Authorization": "Bearer tok-a"})
		_ = s.isAuthorized(r)
	}
	<-done
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

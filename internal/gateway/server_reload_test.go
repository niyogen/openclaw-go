package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPickRequestTraceIDUsesClientHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-ID", "  my-correlation-1  ")
	id := pickRequestTraceID(r)
	if id != "my-correlation-1" {
		t.Fatalf("got %q", id)
	}
}

func TestPickRequestTraceIDTruncatesLongHeader(t *testing.T) {
	long := strings.Repeat("a", 200)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-ID", long)
	id := pickRequestTraceID(r)
	if len(id) != 128 {
		t.Fatalf("len %d", len(id))
	}
}

func TestMetricsRequireAuthToggleSequence(t *testing.T) {
	s := buildTestServer(t, "tok")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	s.SetMetricsRequireAuth(true)
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", resp.StatusCode)
	}

	s.SetMetricsRequireAuth(false)
	resp2, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("want 200 got %d", resp2.StatusCode)
	}
}

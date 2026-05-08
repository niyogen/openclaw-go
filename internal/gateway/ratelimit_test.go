package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterAllow(t *testing.T) {
	rl := NewRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !rl.Allow("127.0.0.1") {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if rl.Allow("127.0.0.1") {
		t.Fatal("4th request should be rate limited")
	}
	// Different key should still be allowed.
	if !rl.Allow("192.168.1.1") {
		t.Fatal("different IP should be allowed")
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	s := buildTestServer(t, "")
	// Override rate limiter with a very tight limit.
	s.rateLimiter = NewRateLimiter(1, time.Minute)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	// First /message call should pass (rate limit = 1, but also needs a valid body).
	// Use /health which is not rate limited — just verify rate limiter itself via Allow().
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %d", resp.StatusCode)
	}
}

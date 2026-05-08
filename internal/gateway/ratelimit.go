package gateway

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// rateLimitEntry tracks request count and window for a single key (IP).
type rateLimitEntry struct {
	count    int
	windowAt time.Time
}

// RateLimiter is a simple per-IP sliding-window rate limiter.
type RateLimiter struct {
	mu        sync.Mutex
	entries   map[string]*rateLimitEntry
	limit     int           // max requests per window
	window    time.Duration // window duration
	lastPrune time.Time
}

// NewRateLimiter creates a limiter allowing limit requests per window per IP.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	if limit <= 0 {
		limit = 60
	}
	if window <= 0 {
		window = time.Minute
	}
	return &RateLimiter{
		entries:   map[string]*rateLimitEntry{},
		limit:     limit,
		window:    window,
		lastPrune: time.Now(),
	}
}

// Allow returns true if the request from key should be allowed.
func (r *RateLimiter) Allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	entry, ok := r.entries[key]
	if !ok || now.Sub(entry.windowAt) > r.window {
		r.entries[key] = &rateLimitEntry{count: 1, windowAt: now}
		r.maybePrune(now)
		return true
	}
	entry.count++
	return entry.count <= r.limit
}

func (r *RateLimiter) maybePrune(now time.Time) {
	if now.Sub(r.lastPrune) < 5*time.Minute {
		return
	}
	r.lastPrune = now
	for k, v := range r.entries {
		if now.Sub(v.windowAt) > r.window {
			delete(r.entries, k)
		}
	}
}

// clientIP extracts the best available IP from a request.
func clientIP(r *http.Request) string {
	if fwd := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); fwd != "" {
		return strings.SplitN(fwd, ",", 2)[0]
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		return addr[:idx]
	}
	return addr
}

// withRateLimit wraps a handler to enforce per-IP rate limiting.
func (s *Server) withRateLimit(next http.HandlerFunc) http.HandlerFunc {
	if s.rateLimiter == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.rateLimiter.Allow(clientIP(r)) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}
		next(w, r)
	}
}

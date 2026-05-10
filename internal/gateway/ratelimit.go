package gateway

import (
	"fmt"
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

// allowResult carries the decision plus state for rate-limit response headers.
type allowResult struct {
	allowed   bool
	remaining int       // requests remaining in the current window
	resetAt   time.Time // when the current window resets
}

// Allow returns true if the request from key should be allowed.
func (r *RateLimiter) Allow(key string) bool {
	res := r.AllowWithInfo(key)
	return res.allowed
}

// AllowWithInfo returns the full rate-limit result including remaining and reset.
func (r *RateLimiter) AllowWithInfo(key string) allowResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	entry, ok := r.entries[key]
	if !ok || now.Sub(entry.windowAt) > r.window {
		e := &rateLimitEntry{count: 1, windowAt: now}
		r.entries[key] = e
		r.maybePrune(now)
		return allowResult{allowed: true, remaining: r.limit - 1, resetAt: now.Add(r.window)}
	}
	entry.count++
	remaining := r.limit - entry.count
	if remaining < 0 {
		remaining = 0
	}
	return allowResult{
		allowed:   entry.count <= r.limit,
		remaining: remaining,
		resetAt:   entry.windowAt.Add(r.window),
	}
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
// It also sets standard X-RateLimit-* response headers.
func (s *Server) withRateLimit(next http.HandlerFunc) http.HandlerFunc {
	if s.rateLimiter == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		res := s.rateLimiter.AllowWithInfo(clientIP(r))
		w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", s.rateLimiter.limit))
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", res.remaining))
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", res.resetAt.Unix()))
		if !res.allowed {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}
		next(w, r)
	}
}

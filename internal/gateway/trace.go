package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// traceKey is the context key for a request's trace ID.
type traceKey struct{}

var traceSeq uint64

// newTraceID generates a unique ID for each request.
func newTraceID() string {
	seq := atomic.AddUint64(&traceSeq, 1)
	return fmt.Sprintf("req-%d-%d", time.Now().UnixNano(), seq)
}

// pickRequestTraceID returns X-Request-ID from the client when sane, else a new id.
func pickRequestTraceID(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("X-Request-ID"))
	if raw == "" {
		return newTraceID()
	}
	raw = strings.ReplaceAll(raw, "\n", "")
	raw = strings.ReplaceAll(raw, "\r", "")
	if len(raw) > 128 {
		raw = raw[:128]
	}
	return raw
}

// withTrace is a middleware that attaches a trace ID to every request and
// logs entry/exit to the gateway log store.
func (s *Server) withTrace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := pickRequestTraceID(r)
		ctx := context.WithValue(r.Context(), traceKey{}, id)
		r = r.WithContext(ctx)

		// Security headers.
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}

		start := time.Now()
		_ = s.logs.Append("debug", "trace", fmt.Sprintf("→ %s %s", r.Method, r.URL.Path), map[string]any{
			"requestId": id,
			"remote":    clientIP(r, s.trustedProxiesSnapshot()),
		})

		rw := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)

		_ = s.logs.Append("debug", "trace", fmt.Sprintf("← %d %s %s (%s)", rw.status, r.Method, r.URL.Path, time.Since(start)), map[string]any{
			"requestId":  id,
			"statusCode": rw.status,
			"durationMs": time.Since(start).Milliseconds(),
		})
	})
}

// statusRecorder captures the HTTP status code written by a handler.
// It forwards Flush() so SSE streaming continues to work.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher, forwarding to the underlying ResponseWriter.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying ResponseWriter's hijacker so
// WebSocket upgrades (gorilla/websocket) can take over the connection
// even when the trace middleware has wrapped the writer.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("response writer does not implement http.Hijacker")
}

// Unwrap exposes the underlying ResponseWriter so http.NewResponseController
// can reach through this wrapper to call Hijack/Flush/SetWriteDeadline/etc.
// Required by Go 1.20+ middleware conventions.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// TraceIDFromContext returns the request trace ID, or "" if not present.
func TraceIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(traceKey{}).(string); ok {
		return id
	}
	return ""
}

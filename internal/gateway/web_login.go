package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// webLoginStatus is the lifecycle state of a single web login attempt.
type webLoginStatus string

const (
	webLoginPending  webLoginStatus = "pending"
	webLoginApproved webLoginStatus = "approved"
	webLoginRejected webLoginStatus = "rejected"
	webLoginExpired  webLoginStatus = "expired"
)

// webLoginAttempt records one in-flight `web.login.start` request and the
// eventual decision driven by a browser visit.
type webLoginAttempt struct {
	Nonce       string         `json:"nonce"`
	CreatedAt   time.Time      `json:"createdAt"`
	ExpiresAt   time.Time      `json:"expiresAt"`
	Status      webLoginStatus `json:"status"`
	IssuedToken string         `json:"issuedToken,omitempty"`
	done        chan struct{}
}

// webLoginRegistry is a lightweight in-memory queue. We do NOT persist these:
// nonces are single-use and short-lived (default 5 min), so a server restart
// invalidating pending logins is the correct security posture.
type webLoginRegistry struct {
	mu       sync.Mutex
	attempts map[string]*webLoginAttempt
}

func newWebLoginRegistry() *webLoginRegistry {
	return &webLoginRegistry{attempts: map[string]*webLoginAttempt{}}
}

// start creates a new pending attempt and returns it.
func (r *webLoginRegistry) start(ttl time.Duration) (*webLoginAttempt, error) {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	nonce, err := generateNonce()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	attempt := &webLoginAttempt{
		Nonce:     nonce,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
		Status:    webLoginPending,
		done:      make(chan struct{}),
	}
	r.mu.Lock()
	r.attempts[nonce] = attempt
	r.pruneLocked()
	r.mu.Unlock()
	return attempt, nil
}

// get returns the named attempt and whether it exists. The returned pointer
// must not be mutated outside the registry's lock.
func (r *webLoginRegistry) get(nonce string) (*webLoginAttempt, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.attempts[nonce]
	return a, ok
}

// decide finalises an attempt. Returns the issued token on approval (or
// empty string on rejection). Repeated decisions on the same nonce error.
func (r *webLoginRegistry) decide(nonce string, approve bool) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.attempts[nonce]
	if !ok {
		return "", errors.New("login attempt not found")
	}
	if a.Status != webLoginPending {
		return "", errors.New("login attempt already decided")
	}
	if time.Now().After(a.ExpiresAt) {
		a.Status = webLoginExpired
		close(a.done)
		return "", errors.New("login attempt expired")
	}
	if !approve {
		a.Status = webLoginRejected
		close(a.done)
		return "", nil
	}
	token, err := generateNonce()
	if err != nil {
		return "", err
	}
	a.Status = webLoginApproved
	a.IssuedToken = token
	close(a.done)
	return token, nil
}

// wait blocks until the attempt is decided, the context cancels, or the
// attempt's ExpiresAt fires. Returns the final status snapshot.
func (r *webLoginRegistry) wait(ctx context.Context, nonce string) (webLoginAttempt, error) {
	r.mu.Lock()
	a, ok := r.attempts[nonce]
	r.mu.Unlock()
	if !ok {
		return webLoginAttempt{}, errors.New("login attempt not found")
	}

	d := time.Until(a.ExpiresAt)
	if d <= 0 {
		// Mark expired and return the snapshot rather than blocking forever.
		_, _ = r.decide(nonce, false)
		return r.snapshot(nonce), nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return webLoginAttempt{}, ctx.Err()
	case <-a.done:
		return r.snapshot(nonce), nil
	case <-timer.C:
		r.mu.Lock()
		if a.Status == webLoginPending {
			a.Status = webLoginExpired
			close(a.done)
		}
		r.mu.Unlock()
		return r.snapshot(nonce), nil
	}
}

// snapshot returns a copy without the done channel.
func (r *webLoginRegistry) snapshot(nonce string) webLoginAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.attempts[nonce]
	if !ok {
		return webLoginAttempt{}
	}
	cp := *a
	cp.done = nil
	return cp
}

// pruneLocked removes attempts older than 10 minutes past expiry. Caller
// holds r.mu.
func (r *webLoginRegistry) pruneLocked() {
	cutoff := time.Now().Add(-10 * time.Minute)
	for k, a := range r.attempts {
		if a.ExpiresAt.Before(cutoff) {
			delete(r.attempts, k)
		}
	}
}

// generateNonce returns a 32-byte hex-encoded random string (256 bits of
// entropy). The same primitive is reused for issued tokens because both
// need to be unguessable.
func generateNonce() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// handleWebLoginPage serves a minimal HTML confirm page so a user landing on
// the URL in a browser can approve the login. The page renders nothing
// sensitive — the actual decision is recorded by handleWebLoginConfirm.
func (s *Server) handleWebLoginPage(w http.ResponseWriter, r *http.Request) {
	nonce := strings.TrimPrefix(r.URL.Path, "/web/login/")
	nonce = strings.TrimSuffix(nonce, "/")
	if nonce == "" {
		http.NotFound(w, r)
		return
	}
	attempt, ok := s.webLogins.get(nonce)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Cache-Control prevents the browser from re-rendering a stale confirm
	// page after the nonce has been decided.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	status := string(attempt.Status)
	_, _ = w.Write([]byte(webLoginPageHTML(nonce, status)))
}

// handleWebLoginConfirm records the user's decision. If auth is enabled, the
// confirming request must itself be authorized (this is a token-rotation
// flow); otherwise (initial setup) anyone reaching the gateway can confirm,
// which is the documented behavior for first-time loopback setup.
func (s *Server) handleWebLoginConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/web/login/")
	rest = strings.TrimSuffix(rest, "/confirm")
	nonce := strings.TrimSuffix(rest, "/")
	if nonce == "" {
		http.NotFound(w, r)
		return
	}
	// If auth is already configured, require the confirmation request to be
	// authenticated — otherwise an unauthenticated attacker who can reach the
	// gateway could rotate tokens out from under the legitimate user.
	if s.authEnabledSnapshot() && !s.isAuthorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// Parse optional approve=false from form/JSON.
	approve := true
	if r.URL.Query().Get("approve") == "false" {
		approve = false
	}
	token, err := s.webLogins.decide(nonce, approve)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"status": string(s.webLogins.snapshot(nonce).Status),
		"token":  token, // empty on rejection
	})
}

func webLoginPageHTML(nonce, status string) string {
	// Single-quoted, inlined HTML keeps us in stdlib-only territory.
	// The form posts to the same URL with `/confirm` appended; the user's
	// existing browser auth (cookies, basic auth) is forwarded automatically.
	form := `<form method="post" action="/web/login/` + nonce + `/confirm" style="display:inline">` +
		`<button type="submit" style="padding:8px 16px;font-size:14px">Approve</button></form>` +
		`<form method="post" action="/web/login/` + nonce + `/confirm?approve=false" style="display:inline;margin-left:8px">` +
		`<button type="submit" style="padding:8px 16px;font-size:14px">Reject</button></form>`
	if status != string(webLoginPending) {
		form = `<p>Status: <strong>` + status + `</strong></p>`
	}
	return `<!DOCTYPE html><html><head><meta charset="utf-8"><title>openclaw login</title></head>` +
		`<body style="font-family:system-ui;max-width:480px;margin:48px auto;padding:16px;line-height:1.5">` +
		`<h2>Approve gateway login</h2>` +
		`<p>A CLI session is requesting access. Approve to issue a new bearer token.</p>` +
		form +
		`</body></html>`
}

// rpcWebLoginStart implements the `web.login.start` RPC.
func (s *Server) rpcWebLoginStart(params json.RawMessage) (any, *rpcError) {
	var p struct {
		TTLSeconds int `json:"ttlSeconds"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	ttl := time.Duration(p.TTLSeconds) * time.Second
	attempt, err := s.webLogins.start(ttl)
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	return map[string]any{
		"nonce":     attempt.Nonce,
		"url":       "/web/login/" + attempt.Nonce,
		"expiresAt": attempt.ExpiresAt,
	}, nil
}

// rpcWebLoginWait implements the `web.login.wait` RPC.
func (s *Server) rpcWebLoginWait(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.Nonce) == "" {
		return nil, &rpcError{Code: -32602, Message: "nonce is required"}
	}
	final, err := s.webLogins.wait(ctx, strings.TrimSpace(p.Nonce))
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	return final, nil
}

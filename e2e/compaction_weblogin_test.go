package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestE2E_SessionCompactionLifecycle drives the full compaction RPC suite
// (added 2026-05-12) against a real in-process gateway: send a few messages,
// compact, list, get, restore, branch. Asserts the gateway, store, and RPC
// dispatcher all agree on the lifecycle.
func TestE2E_SessionCompactionLifecycle(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	// Seed: 5 messages via /message so the agent loop creates a session +
	// echo replies. After this the session has 10 messages (5 user + 5 echo).
	for range 5 {
		resp := h.post(t, "/message", map[string]string{
			"sessionId": "sess-compact",
			"channel":   "cli",
			"message":   "seed message",
		})
		resp.Body.Close()
	}

	// Compact to keep only the last 2 messages.
	resp := h.post(t, "/sessions/sess-compact/compact", map[string]any{"keepN": 2})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact status: %d", resp.StatusCode)
	}

	// List compactions via RPC.
	listEnv := rpcNew(t, h, "sessions.compaction.list", map[string]string{"sessionId": "sess-compact"})
	list, ok := listEnv["result"].([]any)
	if !ok {
		t.Fatalf("compaction.list result wrong shape: %+v", listEnv)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 compaction record, got %d", len(list))
	}
	rec := list[0].(map[string]any)
	id, _ := rec["id"].(string)
	if id == "" || !strings.HasPrefix(id, "cmp_") {
		t.Fatalf("compaction id wrong: %v", rec["id"])
	}

	// Get the single record by ID and confirm PreMessages came back.
	getEnv := rpcNew(t, h, "sessions.compaction.get", map[string]string{"id": id})
	got, _ := getEnv["result"].(map[string]any)
	pre, _ := got["preMessages"].([]any)
	if len(pre) < 5 {
		t.Fatalf("expected pre-compaction messages, got %d", len(pre))
	}

	// Branch into a new session — original should remain untouched.
	branchEnv := rpcNew(t, h, "sessions.compaction.branch", map[string]any{
		"id":           id,
		"newSessionId": "sess-compact-fork",
	})
	branch, _ := branchEnv["result"].(map[string]any)
	if branch["id"] != "sess-compact-fork" {
		t.Fatalf("branch id wrong: %v", branch["id"])
	}

	// Restore on the original; messages count should bounce back to pre-compact size.
	restoreEnv := rpcNew(t, h, "sessions.compaction.restore", map[string]string{"id": id})
	if restoreEnv["error"] != nil {
		t.Fatalf("restore error: %+v", restoreEnv["error"])
	}

	// Fetch history and verify it matches pre-compaction length. The
	// handler returns `{sessionId, history: []Message}` — not the flat
	// `messages` key one might guess.
	hist := h.get(t, "/sessions/sess-compact/history")
	defer hist.Body.Close()
	var histDoc struct {
		History []any `json:"history"`
	}
	_ = json.NewDecoder(hist.Body).Decode(&histDoc)
	if len(histDoc.History) < 5 {
		t.Fatalf("post-restore history should have ≥5 messages, got %d", len(histDoc.History))
	}
}

// TestE2E_WebLoginFlow drives the web.login.* RPCs through the real gateway
// HTTP layer. Uses the open-confirm path (no auth configured), exercising
// start → confirm → wait end-to-end.
func TestE2E_WebLoginFlow(t *testing.T) {
	h := newHarness(t, "") // no auth token → confirm endpoint open per design
	defer h.close()

	// Start an attempt.
	startEnv := rpcNew(t, h, "web.login.start", map[string]any{})
	start, _ := startEnv["result"].(map[string]any)
	nonce, _ := start["nonce"].(string)
	url, _ := start["url"].(string)
	if nonce == "" || url == "" {
		t.Fatalf("start payload missing nonce/url: %+v", start)
	}
	if !strings.HasPrefix(url, "/web/login/") {
		t.Fatalf("unexpected approval url: %q", url)
	}

	// Hit the GET page — should render the inline HTML form.
	pageResp := h.get(t, url)
	defer pageResp.Body.Close()
	if pageResp.StatusCode != http.StatusOK {
		t.Fatalf("page status: %d", pageResp.StatusCode)
	}

	// Confirm via POST — auth is disabled in this harness so the confirm
	// endpoint accepts without a bearer token.
	confirmResp := h.post(t, url+"/confirm", "")
	defer confirmResp.Body.Close()
	if confirmResp.StatusCode != http.StatusOK {
		t.Fatalf("confirm status: %d", confirmResp.StatusCode)
	}
	var confirm struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
		Token  string `json:"token"`
	}
	_ = json.NewDecoder(confirmResp.Body).Decode(&confirm)
	if !confirm.OK || confirm.Status != "approved" || confirm.Token == "" {
		t.Fatalf("confirm payload wrong: %+v", confirm)
	}

	// web.login.wait should now return immediately with the approved snapshot.
	waitEnv := rpcNew(t, h, "web.login.wait", map[string]string{"nonce": nonce})
	wait, _ := waitEnv["result"].(map[string]any)
	if wait["status"] != "approved" {
		t.Fatalf("wait status wrong: %v (full: %+v)", wait["status"], wait)
	}
	if wait["issuedToken"] != confirm.Token {
		t.Fatalf("issuedToken mismatch between confirm and wait: %v vs %v",
			confirm.Token, wait["issuedToken"])
	}
}

// TestE2E_WebLoginRejectedFlow proves the reject path also closes cleanly.
func TestE2E_WebLoginRejectedFlow(t *testing.T) {
	h := newHarness(t, "")
	defer h.close()

	startEnv := rpcNew(t, h, "web.login.start", map[string]any{})
	start, _ := startEnv["result"].(map[string]any)
	nonce, _ := start["nonce"].(string)
	url, _ := start["url"].(string)

	// Reject via query param (?approve=false).
	rejectResp := h.post(t, url+"/confirm?approve=false", "")
	rejectResp.Body.Close()

	waitEnv := rpcNew(t, h, "web.login.wait", map[string]string{"nonce": nonce})
	wait, _ := waitEnv["result"].(map[string]any)
	if wait["status"] != "rejected" {
		t.Fatalf("expected rejected, got %v", wait["status"])
	}
	if wait["issuedToken"] != nil && wait["issuedToken"] != "" {
		t.Fatalf("rejected flow should not issue token, got %v", wait["issuedToken"])
	}
}

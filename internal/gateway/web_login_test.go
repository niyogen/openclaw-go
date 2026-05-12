package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebLoginStartReturnsURLAndNonce(t *testing.T) {
	reg := newWebLoginRegistry()
	attempt, err := reg.start(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempt.Nonce) != 64 { // 32 bytes hex-encoded
		t.Fatalf("nonce length: got %d want 64", len(attempt.Nonce))
	}
	if attempt.Status != webLoginPending {
		t.Fatalf("status: got %q want pending", attempt.Status)
	}
}

func TestWebLoginApproveFlow(t *testing.T) {
	reg := newWebLoginRegistry()
	attempt, _ := reg.start(10 * time.Second)

	// Background waiter — simulates the CLI long-polling for a decision.
	type waitResult struct {
		final webLoginAttempt
		err   error
	}
	results := make(chan waitResult, 1)
	go func() {
		final, err := reg.wait(context.Background(), attempt.Nonce)
		results <- waitResult{final, err}
	}()

	// Allow the waiter to register before we decide. The deterministic way
	// is to call decide synchronously on the main goroutine — the channel
	// signal makes the waiter return either way.
	token, err := reg.decide(attempt.Nonce, true)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("expected non-empty issued token")
	}

	res := <-results
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.final.Status != webLoginApproved {
		t.Fatalf("status: got %q want approved", res.final.Status)
	}
	if res.final.IssuedToken != token {
		t.Fatalf("issued token mismatch: %q vs %q", res.final.IssuedToken, token)
	}
}

func TestWebLoginRejectFlow(t *testing.T) {
	reg := newWebLoginRegistry()
	attempt, _ := reg.start(10 * time.Second)
	token, err := reg.decide(attempt.Nonce, false)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		t.Fatalf("rejection should issue no token; got %q", token)
	}
	final, _ := reg.wait(context.Background(), attempt.Nonce)
	if final.Status != webLoginRejected {
		t.Fatalf("status: got %q want rejected", final.Status)
	}
}

func TestWebLoginDoubleDecideErrors(t *testing.T) {
	reg := newWebLoginRegistry()
	attempt, _ := reg.start(10 * time.Second)
	if _, err := reg.decide(attempt.Nonce, true); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.decide(attempt.Nonce, false); err == nil {
		t.Fatal("expected error on second decide")
	}
}

func TestWebLoginExpires(t *testing.T) {
	reg := newWebLoginRegistry()
	attempt, _ := reg.start(20 * time.Millisecond)
	// Wait past the expiry, then attempt to decide; should error.
	time.Sleep(60 * time.Millisecond)
	if _, err := reg.decide(attempt.Nonce, true); err == nil {
		t.Fatal("expected expired error")
	}
	final, _ := reg.wait(context.Background(), attempt.Nonce)
	if final.Status != webLoginExpired {
		t.Fatalf("status: got %q want expired", final.Status)
	}
}

func TestWebLoginPageRendersForKnownNonce(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	attempt, _ := s.webLogins.start(time.Minute)

	resp, err := http.Get(ts.URL + "/web/login/" + attempt.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page status: got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Approve") {
		t.Fatalf("page missing Approve button: %s", body)
	}
	if !strings.Contains(string(body), attempt.Nonce) {
		t.Fatal("page must include the nonce in its form actions")
	}
}

func TestWebLoginPage404ForUnknownNonce(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/web/login/bogus-nonce")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestWebLoginConfirmOpenWhenAuthDisabled(t *testing.T) {
	s := buildTestServer(t, "") // no auth
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	attempt, _ := s.webLogins.start(time.Minute)
	resp, err := http.Post(ts.URL+"/web/login/"+attempt.Nonce+"/confirm", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("confirm status: %d %s", resp.StatusCode, raw)
	}
	var got struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
		Token  string `json:"token"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Status != "approved" || got.Token == "" {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

func TestWebLoginConfirmRequiresAuthWhenEnabled(t *testing.T) {
	s := buildTestServer(t, "shared-secret")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	attempt, _ := s.webLogins.start(time.Minute)

	// Without auth header: must be rejected.
	resp, err := http.Post(ts.URL+"/web/login/"+attempt.Nonce+"/confirm", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated confirm: got %d want 401", resp.StatusCode)
	}

	// With the correct bearer token, the rotation flow completes.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/web/login/"+attempt.Nonce+"/confirm", bytes.NewBufferString(""))
	req.Header.Set("Authorization", "Bearer shared-secret")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp2.Body)
		t.Fatalf("authenticated confirm: %d %s", resp2.StatusCode, raw)
	}
	var got struct {
		Token string `json:"token"`
	}
	raw, _ := io.ReadAll(resp2.Body)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Token == "" {
		t.Fatal("expected issued token in response")
	}
}

func TestWebLoginRejectViaQueryParam(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	attempt, _ := s.webLogins.start(time.Minute)
	resp, err := http.Post(ts.URL+"/web/login/"+attempt.Nonce+"/confirm?approve=false", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("reject confirm: %d %s", resp.StatusCode, raw)
	}
	var got struct {
		Status string `json:"status"`
		Token  string `json:"token"`
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &got)
	if got.Status != "rejected" || got.Token != "" {
		t.Fatalf("expected rejected with empty token; got %+v", got)
	}
}

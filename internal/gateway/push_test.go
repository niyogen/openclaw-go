package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"openclaw-go/internal/push"
	"openclaw-go/internal/runtime"
)

// fakePushSender records every Send call so the integration tests below
// can assert what would have gone to a real push provider without
// hitting one.
type fakePushSender struct {
	mu    sync.Mutex
	sends [][]byte
}

func (f *fakePushSender) Send(_ context.Context, _ push.Subscription, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, append([]byte(nil), payload...))
	return nil
}
func (f *fakePushSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}
func (f *fakePushSender) lastPayload() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sends) == 0 {
		return nil
	}
	return f.sends[len(f.sends)-1]
}

// pushTestSetup wires a Server (via the shared buildTestServer harness)
// to a Service backed by a fakePushSender. Returns the live Server and
// the recorder.
func pushTestSetup(t *testing.T) (*Server, *fakePushSender) {
	t.Helper()
	s := buildTestServer(t, "")
	dir := t.TempDir()
	svc, err := push.NewService(dir, "mailto:test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakePushSender{}
	svc.SetSender(fake)
	s.SetPushService(svc)
	return s, fake
}

func TestPushRPCPublicKey(t *testing.T) {
	s, _ := pushTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "push.publicKey", "params": map[string]any{}}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/rpc", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			PublicKey string `json:"publicKey"`
		} `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Result.PublicKey == "" {
		t.Fatal("public key not returned")
	}
}

func TestPushRPCWebSubscribeUnsubscribe(t *testing.T) {
	s, _ := pushTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	sub := postRPC(t, ts.URL, "push.web.subscribe", map[string]any{
		"endpoint": "https://push.example.com/abc",
		"p256dh":   "p1",
		"auth":     "a1",
		"label":    "Test Phone",
	})
	id, _ := sub["id"].(string)
	if id == "" {
		t.Fatalf("subscribe didn't return an id; got %+v", sub)
	}

	list := postRPCArray(t, ts.URL, "push.web.list", map[string]any{})
	if len(list) != 1 {
		t.Fatalf("expected 1 subscription, got %d", len(list))
	}

	_ = postRPC(t, ts.URL, "push.web.unsubscribe", map[string]any{"id": id})
	list2 := postRPCArray(t, ts.URL, "push.web.list", map[string]any{})
	if len(list2) != 0 {
		t.Fatalf("expected 0 after unsubscribe, got %d", len(list2))
	}
}

func TestPushRPCTestFanout(t *testing.T) {
	s, fake := pushTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	_ = postRPC(t, ts.URL, "push.web.subscribe", map[string]any{"endpoint": "e1", "p256dh": "p", "auth": "a"})
	_ = postRPC(t, ts.URL, "push.web.subscribe", map[string]any{"endpoint": "e2", "p256dh": "p", "auth": "a"})

	_ = postRPC(t, ts.URL, "push.test", map[string]any{"message": "hello-everyone"})

	if fake.count() != 2 {
		t.Fatalf("expected fan-out to 2 subs, got %d", fake.count())
	}
	payload := fake.lastPayload()
	var got map[string]any
	_ = json.Unmarshal(payload, &got)
	if got["message"] != "hello-everyone" || got["kind"] != "push.test" {
		t.Fatalf("payload shape wrong: %+v", got)
	}
}

func TestPushFiresOnApprovalEnqueue(t *testing.T) {
	s, fake := pushTestSetup(t)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	// Register one subscription.
	_ = postRPC(t, ts.URL, "push.web.subscribe", map[string]any{"endpoint": "e1", "p256dh": "p", "auth": "a"})

	// Trigger an approval enqueue directly — the runtime.ApprovalQueue
	// the gateway constructed is the same instance the executor would use.
	s.approvals.Enqueue(&runtime.ApprovalRequest{
		ID:        "test-approval-1",
		SessionID: "sess-x",
		Tool:      "dangerous_tool",
		Status:    runtime.ApprovalPending,
		CreatedAt: time.Now(),
	})

	// The push goroutine fires async; allow up to 2s.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fake.count() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fake.count() != 1 {
		t.Fatalf("expected 1 push fired on Enqueue, got %d", fake.count())
	}
	var payload map[string]any
	_ = json.Unmarshal(fake.lastPayload(), &payload)
	if payload["kind"] != "approval.requested" || payload["id"] != "test-approval-1" {
		t.Fatalf("push payload shape wrong: %+v", payload)
	}
}

func TestPushRPCRejectsWhenPushNotConfigured(t *testing.T) {
	s := buildTestServer(t, "") // NO push service attached.
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "push.publicKey", "params": map[string]any{}}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/rpc", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Error == nil {
		t.Fatal("expected error when push not configured")
	}
}

// postRPC is a small helper for the tests above. Returns the `result`
// field as a map for tests that need to read fields back.
func postRPC(t *testing.T, base, method string, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(base+"/rpc", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Result map[string]any `json:"result"`
		Error  *struct{ Message string }
	}
	_ = json.Unmarshal(raw, &env)
	if env.Error != nil {
		t.Fatalf("rpc %s error: %s (body=%s)", method, env.Error.Message, raw)
	}
	return env.Result
}

// postRPCArray is the same shape as postRPC but expects an array result
// (used by list endpoints).
func postRPCArray(t *testing.T, base, method string, params any) []map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(base+"/rpc", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Result []map[string]any `json:"result"`
	}
	_ = json.Unmarshal(raw, &env)
	return env.Result
}

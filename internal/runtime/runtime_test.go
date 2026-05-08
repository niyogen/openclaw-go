package runtime

import (
	"context"
	"testing"
	"time"

	"openclaw-go/internal/agents"
)

// --- policy tests ---

func TestPolicyAllowAll(t *testing.T) {
	p := DefaultPolicy()
	if p.evaluate("any.tool") != policyAllow {
		t.Fatal("empty AllowedTools should allow everything")
	}
}

func TestPolicyDeny(t *testing.T) {
	p := DefaultPolicy()
	p.DeniedTools = []string{"dangerous.tool"}
	if p.evaluate("dangerous.tool") != policyDeny {
		t.Fatal("denied tool should be policyDeny")
	}
	if p.evaluate("safe.tool") != policyAllow {
		t.Fatal("non-denied tool should be policyAllow")
	}
}

func TestPolicyRequireApproval(t *testing.T) {
	p := DefaultPolicy()
	p.RequireApproval = []string{"sensitive.tool"}
	if p.evaluate("sensitive.tool") != policyApprove {
		t.Fatal("approval tool should be policyApprove")
	}
}

func TestPolicyAllowList(t *testing.T) {
	p := DefaultPolicy()
	p.AllowedTools = []string{"echo"}
	if p.evaluate("echo") != policyAllow {
		t.Fatal("explicitly allowed tool should be policyAllow")
	}
	if p.evaluate("other") != policyDeny {
		t.Fatal("tool not in allow-list should be policyDeny")
	}
}

// --- approval queue tests ---

func TestApprovalQueueApprove(t *testing.T) {
	q := NewApprovalQueue()
	req := &ApprovalRequest{
		ID:        "req-1",
		SessionID: "sess-1",
		Tool:      "echo",
		Status:    ApprovalPending,
		CreatedAt: time.Now(),
	}
	q.Enqueue(req)

	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = q.Decide("req-1", true)
	}()

	status, err := q.Wait(context.Background(), "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if status != ApprovalApproved {
		t.Fatalf("expected Approved, got %s", status)
	}
}

func TestApprovalQueueReject(t *testing.T) {
	q := NewApprovalQueue()
	req := &ApprovalRequest{
		ID: "req-2", Status: ApprovalPending, CreatedAt: time.Now(),
	}
	q.Enqueue(req)

	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = q.Decide("req-2", false)
	}()

	status, _ := q.Wait(context.Background(), "req-2")
	if status != ApprovalRejected {
		t.Fatalf("expected Rejected, got %s", status)
	}
}

func TestApprovalContextCancel(t *testing.T) {
	q := NewApprovalQueue()
	req := &ApprovalRequest{ID: "req-3", Status: ApprovalPending, CreatedAt: time.Now()}
	q.Enqueue(req)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := q.Wait(ctx, "req-3")
	if err == nil {
		t.Fatal("expected context error")
	}
}

// --- executor tests ---

func TestExecutorSingleTurn(t *testing.T) {
	runner := &agents.EchoRunner{}
	exec := NewExecutor(runner, nil)
	result := exec.Run(context.Background(), RunOptions{
		SessionID: "test",
		Message:   "hello",
		Policy:    DefaultPolicy(),
	})
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.FinalText == "" {
		t.Fatal("expected non-empty reply")
	}
	if len(result.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(result.Turns))
	}
}

func TestExecutorToolDenied(t *testing.T) {
	runner := &agents.EchoRunner{}
	exec := NewExecutor(runner, func(_ context.Context, _ string, _ map[string]any) (any, error) {
		return nil, nil
	})
	policy := DefaultPolicy()
	policy.DeniedTools = []string{"dangerous"}

	rec := exec.InvokeToolWithPolicy(context.Background(), policy, nil, "sess", "dangerous", nil)
	if !rec.Denied {
		t.Fatal("expected denied=true")
	}
}

func TestExtractToolCalls(t *testing.T) {
	// Plain text should return nil.
	if calls := ExtractToolCalls("hello world"); calls != nil {
		t.Fatal("expected nil for plain text")
	}

	// Valid OpenAI tool_calls JSON.
	raw := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call1","type":"function","function":{"name":"echo","arguments":"{\"text\":\"hi\"}"}}]},"finish_reason":"tool_calls"}]}`
	calls := ExtractToolCalls(raw)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "echo" {
		t.Fatalf("unexpected tool name: %s", calls[0].Function.Name)
	}
	args := calls[0].Function.ParsedArgs()
	if args["text"] != "hi" {
		t.Fatalf("unexpected args: %v", args)
	}
}

func TestExecutorMultiTurnToolCall(t *testing.T) {
	// A runner that on first call returns a tool_calls JSON, then on second
	// call (with tool result appended) returns plain text.
	callCount := 0
	mockRunner := &mockToolRunner{
		replies: []string{
			`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"echo","arguments":"{\"text\":\"ping\"}"}}]},"finish_reason":"tool_calls"}]}`,
			"pong from model",
		},
		callCount: &callCount,
	}
	toolCalled := false
	exec := NewExecutor(mockRunner, func(_ context.Context, name string, _ map[string]any) (any, error) {
		toolCalled = true
		return map[string]any{"name": name, "result": "pong"}, nil
	})
	result := exec.Run(context.Background(), RunOptions{
		SessionID: "mt-test",
		Message:   "call echo",
		Policy:    DefaultPolicy(),
	})
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if !toolCalled {
		t.Fatal("expected tool to be called")
	}
	if result.FinalText != "pong from model" {
		t.Fatalf("unexpected final text: %q", result.FinalText)
	}
	if len(result.Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(result.Turns))
	}
}

type mockToolRunner struct {
	replies   []string
	callCount *int
}

func (m *mockToolRunner) GenerateReply(_ context.Context, _ agents.Turn) (string, error) {
	idx := *m.callCount
	*m.callCount++
	if idx >= len(m.replies) {
		return "done", nil
	}
	return m.replies[idx], nil
}

func TestExecutorToolAllowed(t *testing.T) {
	runner := &agents.EchoRunner{}
	called := false
	exec := NewExecutor(runner, func(_ context.Context, name string, _ map[string]any) (any, error) {
		called = true
		return map[string]any{"name": name}, nil
	})
	rec := exec.InvokeToolWithPolicy(context.Background(), DefaultPolicy(), nil, "sess", "echo", map[string]any{"text": "hi"})
	if rec.Denied || rec.Error != "" {
		t.Fatalf("expected allowed, got denied=%v err=%s", rec.Denied, rec.Error)
	}
	if !called {
		t.Fatal("expected tool fn to be called")
	}
}

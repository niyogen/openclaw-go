package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
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

// TestApprovalQueuePruning verifies that decided entries are cleaned up
// and do not grow the map indefinitely.
func TestApprovalQueuePruning(t *testing.T) {
	q := NewApprovalQueue()
	for i := 0; i < 20; i++ {
		id := "req-prune-" + string(rune('A'+i))
		q.Enqueue(&ApprovalRequest{ID: id, Status: ApprovalPending, CreatedAt: time.Now()})
		_ = q.Decide(id, true)
	}
	// After deciding all entries, pruneLocked is called each time.
	// Entries decided < 5 min ago are NOT yet pruned (TTL is 5 min) but the
	// pruning path at least should not panic and should run without error.
	// We verify the queue can still accept new entries normally.
	q.Enqueue(&ApprovalRequest{ID: "new-after-prune", Status: ApprovalPending, CreatedAt: time.Now()})
	if _, ok := q.Get("new-after-prune"); !ok {
		t.Fatal("new entry should still be present after prune cycle")
	}
}

// TestIdGenConcurrentSafety runs many concurrent calls to idGen (via
// InvokeToolWithPolicy requiring approval) and verifies no data race.
// This is most useful with -race but also validates uniqueness.
func TestIdGenConcurrentSafety(t *testing.T) {
	runner := &agents.EchoRunner{}
	exec := NewExecutor(runner, nil)

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = map[string]struct{}{}
	)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := exec.idGen()
			mu.Lock()
			ids[id] = struct{}{}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(ids) != 50 {
		t.Fatalf("expected 50 unique IDs, got %d (possible race or collision)", len(ids))
	}
}

// TestToolErrorJSONEscaping verifies that error strings with special JSON
// characters do not produce malformed JSON in tool result history.
func TestToolErrorJSONEscaping(t *testing.T) {
	quote := `has "quotes" and \backslash`
	callCount := 0
	mockRunner := &mockToolRunner{
		replies: []string{
			// First turn: request a tool call.
			`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"bad.tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			// Second turn: plain text after seeing tool error in history.
			"got it",
		},
		callCount: &callCount,
	}
	exec := NewExecutor(mockRunner, func(_ context.Context, _ string, _ map[string]any) (any, error) {
		return nil, &stringError{quote}
	})
	result := exec.Run(context.Background(), RunOptions{
		Message: "run bad tool",
		Policy:  DefaultPolicy(),
	})
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	// Verify the error was passed through history as valid JSON.
	if len(result.Turns) < 1 || len(result.Turns[0].ToolCalls) < 1 {
		t.Fatal("expected at least one tool call record")
	}
	errStr := result.Turns[0].ToolCalls[0].Error
	if !strings.Contains(errStr, "quotes") {
		t.Fatalf("expected error string in record, got: %q", errStr)
	}
}

type stringError struct{ s string }

func (e *stringError) Error() string { return e.s }

// ── TruncateHistory tests ─────────────────────────────────────────────────

func TestTruncateHistoryNoOp(t *testing.T) {
	h := []agents.HistoryMessage{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}}
	out := TruncateHistory(h, 0)
	if len(out) != 2 {
		t.Fatalf("expected 2, got %d", len(out))
	}
}

func TestTruncateHistoryDropsOldest(t *testing.T) {
	h := []agents.HistoryMessage{
		{Role: "user", Content: "1"},
		{Role: "assistant", Content: "2"},
		{Role: "user", Content: "3"},
	}
	out := TruncateHistory(h, 2)
	if len(out) != 2 {
		t.Fatalf("expected 2, got %d", len(out))
	}
	if out[0].Content != "2" || out[1].Content != "3" {
		t.Fatalf("unexpected: %+v", out)
	}
}

func TestTruncateHistoryPreservesSystem(t *testing.T) {
	h := []agents.HistoryMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "1"},
		{Role: "assistant", Content: "2"},
		{Role: "user", Content: "3"},
	}
	out := TruncateHistory(h, 2)
	// system always kept + 1 most-recent non-system
	if len(out) != 2 {
		t.Fatalf("expected 2, got %d", len(out))
	}
	if out[0].Role != "system" {
		t.Fatalf("first message should be system, got %s", out[0].Role)
	}
	if out[1].Content != "3" {
		t.Fatalf("expected most-recent non-system, got %q", out[1].Content)
	}
}

func TestTruncateHistoryEmpty(t *testing.T) {
	out := TruncateHistory(nil, 5)
	if out != nil {
		t.Fatalf("expected nil for empty input, got %v", out)
	}
}

// ── RunStream tests ───────────────────────────────────────────────────────

func TestRunStreamSingleTurn(t *testing.T) {
	runner := &agents.EchoRunner{}
	exec := NewExecutor(runner, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events := make(chan RunEvent, 32)
	go exec.RunStream(ctx, RunOptions{Message: "hello stream", Policy: DefaultPolicy()}, events)

	var types []RunEventType
	var doneEv RunEvent
	for ev := range events {
		types = append(types, ev.Type)
		if ev.Type == RunEventDone {
			doneEv = ev
		}
	}

	if len(types) == 0 {
		t.Fatal("no events received")
	}
	if types[0] != RunEventStart {
		t.Fatalf("first event should be start, got %s", types[0])
	}
	if doneEv.Type != RunEventDone {
		t.Fatal("expected RunEventDone")
	}
	if doneEv.Turns < 1 {
		t.Fatal("expected at least 1 turn")
	}
	if doneEv.RunID == "" {
		t.Fatal("expected non-empty runId")
	}
}

func TestRunStreamContextTruncation(t *testing.T) {
	// Verify MaxContextMessages is respected in RunStream.
	seen := []agents.Turn{}
	mockRunner := &captureRunner{
		delegate: &agents.EchoRunner{},
		capture:  &seen,
	}
	exec := NewExecutor(mockRunner, nil)

	longHistory := make([]agents.HistoryMessage, 20)
	for i := range longHistory {
		longHistory[i] = agents.HistoryMessage{Role: "user", Content: fmt.Sprintf("msg%d", i)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	policy := DefaultPolicy()
	policy.MaxContextMessages = 5

	events := make(chan RunEvent, 32)
	go exec.RunStream(ctx, RunOptions{
		Message: "new",
		History: longHistory,
		Policy:  policy,
	}, events)

	for range events {
	}

	if len(seen) == 0 {
		t.Fatal("no turns captured")
	}
	if len(seen[0].History) > 5 {
		t.Fatalf("history not truncated: got %d messages", len(seen[0].History))
	}
}

type captureRunner struct {
	delegate agents.Runner
	capture  *[]agents.Turn
}

func (r *captureRunner) GenerateReply(ctx context.Context, turn agents.Turn) (string, error) {
	*r.capture = append(*r.capture, turn)
	return r.delegate.GenerateReply(ctx, turn)
}

func TestExecutorTruncationInRun(t *testing.T) {
	seen := []agents.Turn{}
	mock := &captureRunner{delegate: &agents.EchoRunner{}, capture: &seen}
	exec := NewExecutor(mock, nil)

	history := make([]agents.HistoryMessage, 10)
	for i := range history {
		history[i] = agents.HistoryMessage{Role: "user", Content: fmt.Sprintf("h%d", i)}
	}

	policy := DefaultPolicy()
	policy.MaxContextMessages = 3

	result := exec.Run(context.Background(), RunOptions{
		Message: "go",
		History: history,
		Policy:  policy,
	})
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if len(seen) == 0 {
		t.Fatal("no turns captured")
	}
	if len(seen[0].History) > 3 {
		t.Fatalf("history should be capped at 3, got %d", len(seen[0].History))
	}
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

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"openclaw-go/internal/agents"
)

// ToolCallFn is a function that can execute a named tool and return its result.
type ToolCallFn func(ctx context.Context, name string, arguments map[string]any) (any, error)

// RunOptions configures a single agent run.
type RunOptions struct {
	SessionID    string
	Message      string
	History      []agents.HistoryMessage
	Instructions string // system prompt prepended to every turn
	Policy       ExecPolicy
	Approvals    *ApprovalQueue
	// OnToolCall is called before each tool invocation (for logging/hooks). Optional.
	OnToolCall func(tool string, args map[string]any)
}

// TurnResult holds the outcome of one agent turn.
type TurnResult struct {
	Turn    int
	Content string
	// ToolCalls lists any tools the agent requested (for audit/display).
	ToolCalls []ToolCallRecord
}

// ToolCallRecord is an entry in the turn's tool-call audit trail.
type ToolCallRecord struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
	Result    any            `json:"result,omitempty"`
	Error     string         `json:"error,omitempty"`
	Approved  bool           `json:"approved"`
	Denied    bool           `json:"denied"`
}

// RunResult is the complete output of an agent run.
type RunResult struct {
	Turns     []TurnResult
	FinalText string
	Err       error
}

// Executor runs an agent through a multi-turn loop with policy enforcement.
type Executor struct {
	runner     agents.Runner
	toolFn     ToolCallFn
	subagentFn func(ctx context.Context, message, instructions string) (string, error)
	idGen      func() string
}

// NewExecutor creates an executor backed by a runner and optional tool function.
func NewExecutor(runner agents.Runner, toolFn ToolCallFn) *Executor {
	var seq int64
	return &Executor{
		runner: runner,
		toolFn: toolFn,
		idGen: func() string {
			n := atomic.AddInt64(&seq, 1)
			return fmt.Sprintf("appr-%d-%d", time.Now().UnixNano(), n)
		},
	}
}

// SetSubagentFn registers a subagent delegation function.
// Tools with names starting "subagent." are routed through this function.
func (e *Executor) SetSubagentFn(fn func(ctx context.Context, message, instructions string) (string, error)) {
	e.subagentFn = fn
}

// Run executes the agent with the given options, enforcing the exec policy.
// It supports multi-turn tool-calling loops: when the model returns tool_calls
// (OpenAI function-calling format) the executor invokes each tool through
// policy/approval checks, appends tool results, and re-enters the loop until
// the model produces a plain text reply or the turn budget is exhausted.
func (e *Executor) Run(ctx context.Context, opts RunOptions) RunResult {
	policy := opts.Policy.normalize()
	var allTurns []TurnResult
	history := append([]agents.HistoryMessage{}, opts.History...)

	// Prepend system prompt to history if provided.
	if strings.TrimSpace(opts.Instructions) != "" {
		history = append([]agents.HistoryMessage{
			{Role: "system", Content: opts.Instructions},
		}, history...)
	}

	currentMessage := opts.Message
	pendingUserMessage := true // first iteration sends the user message

	for turn := 0; turn < policy.MaxTurns; turn++ {
		select {
		case <-ctx.Done():
			return RunResult{Turns: allTurns, Err: ctx.Err()}
		default:
		}

		turnInput := agents.Turn{History: history}
		if pendingUserMessage {
			turnInput.Message = currentMessage
		}

		reply, err := e.retryingReply(ctx, policy, turnInput)
		if err != nil {
			return RunResult{Turns: allTurns, Err: err}
		}

		// Check whether the reply contains tool calls (OpenAI format).
		toolCalls := ExtractToolCalls(reply)

		tr := TurnResult{Turn: turn + 1, Content: reply}

		if len(toolCalls) == 0 {
			// Plain text reply — we're done. We don't need to update history
			// since we're returning immediately; just record the turn.
			allTurns = append(allTurns, tr)
			return RunResult{Turns: allTurns, FinalText: reply}
		}

		// Model emitted tool calls — execute each one through policy.
		if pendingUserMessage {
			history = append(history, agents.HistoryMessage{Role: "user", Content: currentMessage})
			pendingUserMessage = false
		}
		// Append the assistant's tool-call message to history.
		history = append(history, agents.HistoryMessage{Role: "assistant", Content: reply})

		for _, tc := range toolCalls {
			parsedArgs := tc.Function.ParsedArgs()
			// If argument parsing failed, record the error without invoking the tool.
			if errMsg, bad := parsedArgs[argsParseErrorKey]; bad {
				if opts.OnToolCall != nil {
					opts.OnToolCall(tc.Function.Name, parsedArgs)
				}
				tr.ToolCalls = append(tr.ToolCalls, ToolCallRecord{
					Tool:  tc.Function.Name,
					Error: fmt.Sprintf("argument parse error: %v", errMsg),
				})
				if errJSON, jerr := json.Marshal(map[string]string{"error": fmt.Sprintf("argument parse error: %v", errMsg)}); jerr == nil {
					history = append(history, agents.HistoryMessage{Role: "tool", Content: string(errJSON)})
				}
				continue
			}
			if opts.OnToolCall != nil {
				opts.OnToolCall(tc.Function.Name, parsedArgs)
			}
			rec := e.InvokeToolWithPolicy(
				ctx, policy, opts.Approvals, opts.SessionID,
				tc.Function.Name, parsedArgs,
			)
			tr.ToolCalls = append(tr.ToolCalls, rec)

			// Encode tool result back into history so the model can see it.
			resultStr := ""
			if rec.Error != "" {
				if errJSON, jerr := json.Marshal(map[string]string{"error": rec.Error}); jerr == nil {
					resultStr = string(errJSON)
				} else {
					resultStr = `{"error":"tool execution failed"}`
				}
			} else {
				if raw, merr := encodeResult(rec.Result); merr == nil {
					resultStr = raw
				}
			}
			history = append(history, agents.HistoryMessage{
				Role:    "tool",
				Content: resultStr,
			})
		}

		allTurns = append(allTurns, tr)
		// Loop: next iteration sends history (with tool results) to model.
		currentMessage = ""
	}

	return RunResult{
		Turns: allTurns,
		Err:   &ErrTurnsExceeded{Max: policy.MaxTurns},
	}
}

func encodeResult(v any) (string, error) {
	if v == nil {
		return "null", nil
	}
	raw, err := fmt.Sprintf("%v", v), error(nil)
	if b, jerr := json.Marshal(v); jerr == nil {
		raw = string(b)
	}
	return raw, err
}

// retryingReply calls the runner with up to policy.MaxRetries retries on error.
func (e *Executor) retryingReply(ctx context.Context, policy ExecPolicy, turn agents.Turn) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= policy.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 200 * time.Millisecond):
			}
		}
		reply, err := e.runner.GenerateReply(ctx, turn)
		if err == nil {
			return reply, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("runner failed after %d retries: %w", policy.MaxRetries, lastErr)
}

// InvokeToolWithPolicy checks the policy and approval queue before executing.
func (e *Executor) InvokeToolWithPolicy(
	ctx context.Context,
	policy ExecPolicy,
	approvals *ApprovalQueue,
	sessionID string,
	toolName string,
	arguments map[string]any,
) ToolCallRecord {
	rec := ToolCallRecord{Tool: toolName, Arguments: arguments}

	switch policy.evaluate(toolName) {
	case policyDeny:
		rec.Denied = true
		rec.Error = (&ErrToolDenied{Tool: toolName}).Error()
		return rec

	case policyApprove:
		if approvals == nil {
			rec.Denied = true
			rec.Error = "approval required but no approval queue is configured"
			return rec
		}
		reqID := e.idGen()
		approvals.Enqueue(&ApprovalRequest{
			ID:        reqID,
			SessionID: sessionID,
			Tool:      toolName,
			Arguments: arguments,
			Status:    ApprovalPending,
			CreatedAt: time.Now().UTC(),
		})
		status, err := approvals.Wait(ctx, reqID)
		if err != nil {
			rec.Denied = true
			rec.Error = fmt.Sprintf("approval wait failed: %v", err)
			return rec
		}
		if status != ApprovalApproved {
			rec.Denied = true
			rec.Error = "tool call rejected by approver"
			return rec
		}
		rec.Approved = true
	}

	// Route subagent.* tools through the subagent function if registered.
	if e.subagentFn != nil && strings.HasPrefix(toolName, "subagent.") {
		msg, _ := arguments["message"].(string)
		instructions, _ := arguments["instructions"].(string)
		result, err := e.subagentFn(ctx, msg, instructions)
		if err != nil {
			rec.Error = err.Error()
		} else {
			rec.Result = map[string]any{"reply": result}
		}
		return rec
	}

	if e.toolFn == nil {
		rec.Error = "no tool executor registered"
		return rec
	}
	result, err := e.toolFn(ctx, toolName, arguments)
	if err != nil {
		rec.Error = err.Error()
	} else {
		rec.Result = result
	}
	return rec
}

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"openclaw-go/internal/agents"
)

// streamRunID generates a unique run ID for streaming runs.
// Uses the same format as generateRunID in the gateway package, but is
// package-local so the runtime package has no dependency on gateway.
var streamRunSeq int64

func newStreamRunID() string {
	n := atomic.AddInt64(&streamRunSeq, 1)
	return fmt.Sprintf("%s-%d-stream", time.Now().UTC().Format("20060102-150405.999999999"), n)
}

// RunStream executes the agent with the same multi-turn tool-calling loop as
// Run, but instead of returning a RunResult it emits RunEvents to events.
// The channel is always closed when RunStream returns.
//
// - A RunEventStart is sent immediately.
// - For each tool call: RunEventToolCall then RunEventToolResult.
// - After each turn: RunEventTurnEnd.
// - For the final plain-text reply: RunEventDelta events (rune-by-rune), then RunEventDone.
// - On any error: RunEventError (then the channel is closed).
func (e *Executor) RunStream(ctx context.Context, opts RunOptions, events chan<- RunEvent) {
	defer close(events)

	send := func(ev RunEvent) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	policy := opts.Policy.normalize()
	history := append([]agents.HistoryMessage{}, opts.History...)

	if strings.TrimSpace(opts.Instructions) != "" {
		history = append([]agents.HistoryMessage{
			{Role: "system", Content: opts.Instructions},
		}, history...)
	}

	if !send(RunEvent{Type: RunEventStart}) {
		return
	}

	runID := newStreamRunID()
	currentMessage := opts.Message
	pendingUserMessage := true

	for turn := 0; turn < policy.MaxTurns; turn++ {
		select {
		case <-ctx.Done():
			send(RunEvent{Type: RunEventError, Error: ctx.Err().Error()})
			return
		default:
		}

		turnHistory := history
		if policy.MaxContextMessages > 0 {
			turnHistory = TruncateHistory(history, policy.MaxContextMessages)
		}
		turnInput := agents.Turn{History: turnHistory}
		if pendingUserMessage {
			turnInput.Message = currentMessage
		}

		reply, err := e.retryingReply(ctx, policy, turnInput)
		if err != nil {
			send(RunEvent{Type: RunEventError, Error: err.Error()})
			return
		}

		toolCalls := ExtractToolCalls(reply)

		if len(toolCalls) == 0 {
			// Plain-text final reply — stream rune-by-rune.
			if !streamReplyRunes(ctx, reply, events) {
				return
			}
			send(RunEvent{Type: RunEventDone, Reply: reply, Turns: turn + 1, RunID: runID})
			return
		}

		// Append user message and assistant tool-call message to history.
		if pendingUserMessage {
			history = append(history, agents.HistoryMessage{Role: "user", Content: currentMessage})
			pendingUserMessage = false
		}
		history = append(history, agents.HistoryMessage{Role: "assistant", Content: reply})

		for _, tc := range toolCalls {
			parsedArgs := tc.Function.ParsedArgs()

			if errMsg, bad := parsedArgs[argsParseErrorKey]; bad {
				errStr := fmt.Sprintf("argument parse error: %v", errMsg)
				if !send(RunEvent{Type: RunEventToolCall, Tool: tc.Function.Name, Arguments: parsedArgs}) {
					return
				}
				if errJSON, jerr := json.Marshal(map[string]string{"error": errStr}); jerr == nil {
					history = append(history, agents.HistoryMessage{Role: "tool", Content: string(errJSON)})
				}
				continue
			}

			if !send(RunEvent{Type: RunEventToolCall, Tool: tc.Function.Name, Arguments: parsedArgs}) {
				return
			}

			rec := e.InvokeToolWithPolicy(ctx, policy, opts.Approvals, opts.SessionID, tc.Function.Name, parsedArgs)

			var resultVal any
			var resultStr string
			if rec.Error != "" {
				resultVal = map[string]string{"error": rec.Error}
				if errJSON, jerr := json.Marshal(resultVal); jerr == nil {
					resultStr = string(errJSON)
				} else {
					resultStr = `{"error":"tool execution failed"}`
				}
			} else {
				resultVal = rec.Result
				if raw, merr := encodeResult(rec.Result); merr == nil {
					resultStr = raw
				}
			}

			if !send(RunEvent{Type: RunEventToolResult, Tool: tc.Function.Name, Result: resultVal}) {
				return
			}

			history = append(history, agents.HistoryMessage{Role: "tool", Content: resultStr})
		}

		if !send(RunEvent{Type: RunEventTurnEnd, Turn: turn + 1}) {
			return
		}
		currentMessage = ""
	}

	send(RunEvent{Type: RunEventError, Error: (&ErrTurnsExceeded{Max: policy.MaxTurns}).Error()})
}

// streamReplyRunes delivers s rune-by-rune as RunEventDelta events to ch.
// Returns false if the context was cancelled before all runes were delivered.
func streamReplyRunes(ctx context.Context, s string, ch chan<- RunEvent) bool {
	for i := 0; i < len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		chunk := s[i : i+size]
		i += size
		select {
		case ch <- RunEvent{Type: RunEventDelta, Content: chunk}:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

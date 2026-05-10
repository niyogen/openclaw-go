package runtime

// RunEventType identifies the kind of event emitted during a streaming run.
type RunEventType string

const (
	RunEventStart      RunEventType = "start"
	RunEventDelta      RunEventType = "delta"
	RunEventToolCall   RunEventType = "tool_call"
	RunEventToolResult RunEventType = "tool_result"
	RunEventTurnEnd    RunEventType = "turn_end"
	RunEventDone       RunEventType = "done"
	RunEventError      RunEventType = "error"
)

// RunEvent is a single event emitted by Executor.RunStream.
type RunEvent struct {
	Type      RunEventType   `json:"type"`
	Content   string         `json:"content,omitempty"`   // for delta
	Tool      string         `json:"tool,omitempty"`      // for tool_call/result
	Arguments map[string]any `json:"arguments,omitempty"` // for tool_call
	Result    any            `json:"result,omitempty"`    // for tool_result
	Turn      int            `json:"turn,omitempty"`      // for turn_end
	Reply     string         `json:"reply,omitempty"`     // for done
	Turns     int            `json:"turns,omitempty"`     // for done
	RunID     string         `json:"runId,omitempty"`     // for done
	Error     string         `json:"error,omitempty"`     // for error
}

package runtime

import (
	"encoding/json"
	"strings"
)

// ToolCallRequest is the structured tool-call emitted by a model
// when it wants to invoke a tool (OpenAI function-calling format).
type ToolCallRequest struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function ToolCallFunc `json:"function"`
}

// ToolCallFunc carries the function name and serialised arguments.
type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded map
}

// argsParseErrorKey is a sentinel key injected into the parsed-args map when
// the model-supplied arguments JSON is malformed.  It uses the zero-width
// joiner so it cannot appear in any real JSON key produced by a model.
const argsParseErrorKey = "\u200b_openclaw_parse_error"

// ParsedArgs deserialises the JSON arguments string into a map.
// If the arguments string is malformed the returned map contains
// argsParseErrorKey so callers can reject the tool call instead of
// silently invoking it with empty arguments.
func (f ToolCallFunc) ParsedArgs() map[string]any {
	if strings.TrimSpace(f.Arguments) == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(f.Arguments), &m); err != nil {
		return map[string]any{argsParseErrorKey: err.Error()}
	}
	return m
}

// openAIResponseWithTools is the shape of a chat completion that may contain tool calls.
type openAIResponseWithTools struct {
	Choices []struct {
		Message struct {
			Role      string            `json:"role"`
			Content   *string           `json:"content"`
			ToolCalls []ToolCallRequest `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// ExtractToolCalls tries to parse raw JSON from a model reply as an OpenAI
// chat completion that contains tool_calls, returning the slice if found.
// If the reply is plain text or doesn't parse, it returns nil.
func ExtractToolCalls(raw string) []ToolCallRequest {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	var resp openAIResponseWithTools
	if err := json.Unmarshal([]byte(trimmed), &resp); err != nil {
		return nil
	}
	if len(resp.Choices) == 0 {
		return nil
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) == 0 {
		return nil
	}
	return calls
}

// ToolResultMessage formats tool call results back into the history
// in the OpenAI tool-result format.
type ToolResultMessage struct {
	Role       string `json:"role"` // "tool"
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

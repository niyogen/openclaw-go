package agents

import (
	"strings"
	"testing"
)

func TestAnthropicAlternationEnforced(t *testing.T) {
	turn := Turn{
		History: []HistoryMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi there"},
			{Role: "user", Content: "second"},
			{Role: "user", Content: "third"}, // consecutive user — must be merged
		},
		Message: "final",
	}
	msgs := buildAnthropicMessages(turn)
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == msgs[i-1].Role {
			t.Fatalf("consecutive same-role at index %d: role=%s", i, msgs[i].Role)
		}
	}
}

func TestAnthropicToolResultConvertedToUser(t *testing.T) {
	turn := Turn{
		History: []HistoryMessage{
			{Role: "user", Content: "run tool"},
			{Role: "assistant", Content: `{"tool_calls":[]}`},
			{Role: "tool", Content: `{"result":"ok"}`},
		},
		Message: "continue",
	}
	msgs := buildAnthropicMessages(turn)
	for _, m := range msgs {
		if m.Role == "tool" {
			t.Fatal("'tool' role should not appear in Anthropic messages")
		}
	}
	// The tool result should be folded into a user message.
	foundToolResult := false
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(m.Content, "[tool result]") {
			foundToolResult = true
		}
	}
	if !foundToolResult {
		t.Fatal("tool result content not found in any user message")
	}
}

func TestAnthropicSystemContextPrependedToUser(t *testing.T) {
	turn := Turn{
		History: []HistoryMessage{
			{Role: "system", Content: "You are helpful."},
		},
		Message: "hello",
	}
	msgs := buildAnthropicMessages(turn)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	if msgs[0].Role != "user" {
		t.Fatalf("first message should be user, got %s", msgs[0].Role)
	}
	if !strings.Contains(msgs[0].Content, "You are helpful.") {
		t.Fatalf("system context not prepended to user message: %q", msgs[0].Content)
	}
}

func TestAnthropicEmptyContentSkipped(t *testing.T) {
	turn := Turn{
		History: []HistoryMessage{
			{Role: "user", Content: ""},    // should be skipped
			{Role: "assistant", Content: "response"},
		},
		Message: "next",
	}
	msgs := buildAnthropicMessages(turn)
	for _, m := range msgs {
		if m.Content == "" {
			t.Fatalf("empty content message should be skipped: role=%s", m.Role)
		}
	}
}

func TestAnthropicCurrentMessageAlwaysLastUser(t *testing.T) {
	turn := Turn{
		History: []HistoryMessage{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "reply"},
		},
		Message: "second user turn",
	}
	msgs := buildAnthropicMessages(turn)
	last := msgs[len(msgs)-1]
	if last.Role != "user" {
		t.Fatalf("last message should be user, got %s", last.Role)
	}
	if !strings.Contains(last.Content, "second user turn") {
		t.Fatalf("last message should contain current message, got %q", last.Content)
	}
}

package agents

import (
	"context"
	"strings"
	"testing"
)

func TestSimulatedStream(t *testing.T) {
	runner := &EchoRunner{}
	out := make(chan StreamChunk, 64)
	go func() {
		SimulatedStream(context.Background(), runner, Turn{Message: "hello world"}, out)
		close(out)
	}()

	var parts []string
	done := false
	for chunk := range out {
		if chunk.Err != nil {
			t.Fatalf("unexpected error: %v", chunk.Err)
		}
		if chunk.Done {
			done = true
			continue
		}
		parts = append(parts, chunk.Delta)
	}
	if !done {
		t.Fatal("stream did not complete with Done=true")
	}
	combined := strings.TrimSpace(strings.Join(parts, ""))
	if !strings.Contains(combined, "hello") {
		t.Fatalf("expected reply to contain 'hello', got: %q", combined)
	}
}

func TestStreamFallback(t *testing.T) {
	// EchoRunner does not implement StreamingRunner — should fall back to SimulatedStream.
	runner := &EchoRunner{}
	out := make(chan StreamChunk, 64)
	go func() {
		Stream(context.Background(), runner, Turn{Message: "ping"}, out)
		close(out)
	}()

	var chunks []StreamChunk
	for c := range out {
		chunks = append(chunks, c)
	}
	if len(chunks) == 0 {
		t.Fatal("no chunks received")
	}
	last := chunks[len(chunks)-1]
	if !last.Done {
		t.Fatal("expected last chunk to be Done=true")
	}
}

package agents

import (
	"context"
	"unicode/utf8"
)

// StreamChunk is a single token/chunk delivered during streaming.
type StreamChunk struct {
	Delta string // the incremental text
	Done  bool   // true on the final chunk (Delta may be empty)
	Err   error  // non-nil if the stream encountered an error
}

// StreamingRunner is an optional interface that runners may implement to
// support incremental token delivery.  If a runner does not implement this
// interface the gateway falls back to buffering the full reply and emitting
// it as a single chunk.
type StreamingRunner interface {
	Runner
	StreamReply(ctx context.Context, turn Turn, out chan<- StreamChunk)
}

// SimulatedStream wraps any Runner and delivers the full reply as a single
// chunk followed by a done marker.  This ensures every runner is compatible
// with the streaming code path without requiring SSE-aware changes.
func SimulatedStream(ctx context.Context, r Runner, turn Turn, out chan<- StreamChunk) {
	reply, err := r.GenerateReply(ctx, turn)
	if err != nil {
		out <- StreamChunk{Err: err}
		return
	}
	// Deliver rune-by-rune so clients see progressive output without collapsing
	// whitespace or newlines (strings.Fields would lose formatting).
	for i := 0; i < len(reply); {
		_, size := utf8.DecodeRuneInString(reply[i:])
		out <- StreamChunk{Delta: reply[i : i+size]}
		i += size
	}
	out <- StreamChunk{Done: true}
}

// Stream delivers chunks from the runner.  If the runner implements
// StreamingRunner its native method is used; otherwise SimulatedStream
// is used as fallback.
func Stream(ctx context.Context, r Runner, turn Turn, out chan<- StreamChunk) {
	if sr, ok := r.(StreamingRunner); ok {
		sr.StreamReply(ctx, turn, out)
		return
	}
	SimulatedStream(ctx, r, turn, out)
}

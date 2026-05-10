package channels

import (
	"context"
	"errors"
	"testing"
	"time"
)

type flakySendChannel struct {
	name          string
	failRemaining int
	sendCalls     int
}

func (f *flakySendChannel) Name() string { return f.name }

func (f *flakySendChannel) Send(ctx context.Context, message OutboundMessage) error {
	f.sendCalls++
	if f.failRemaining > 0 {
		f.failRemaining--
		return errors.New("transient")
	}
	return nil
}

func TestRouterDispatch_RetriesWithBackoff(t *testing.T) {
	ch := &flakySendChannel{name: "testch", failRemaining: 2}
	r := NewRouterWithRetries(3)
	r.Register(ch)

	ctx := context.Background()
	msg := OutboundMessage{Channel: "testch", Message: "hi"}

	start := time.Now()
	if err := r.Dispatch(ctx, msg); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	// First attempt + 2 retries => at least 200ms + 400ms backoff
	if elapsed < 500*time.Millisecond {
		t.Fatalf("backoff too short: %v", elapsed)
	}
	if ch.sendCalls != 3 {
		t.Fatalf("sendCalls=%d want 3", ch.sendCalls)
	}
}

func TestRouterDispatch_AllAttemptsFail(t *testing.T) {
	ch := &flakySendChannel{name: "x", failRemaining: 10}
	r := NewRouterWithRetries(2)
	r.Register(ch)

	err := r.Dispatch(context.Background(), OutboundMessage{Channel: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if ch.sendCalls != 3 {
		t.Fatalf("sendCalls=%d want 3", ch.sendCalls)
	}
}

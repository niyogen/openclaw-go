package channels

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFetcher is an in-memory EmailFetcher used by every poller test —
// no real IMAP server needed, no network. Each test stages messages it
// wants delivered, runs the poller, and asserts on the handler captures.
type fakeFetcher struct {
	mu           sync.Mutex
	connectErr   error
	connectCount int32
	closeCount   int32
	// queue of message batches; FetchNew pops the front each call. When
	// empty, FetchNew returns nil (no error) — that's the "no new mail"
	// case which must NOT be treated as an error by the poller.
	queues   [][]EmailMessage
	fetchErr error
}

func (f *fakeFetcher) Connect(_ context.Context) error {
	atomic.AddInt32(&f.connectCount, 1)
	return f.connectErr
}
func (f *fakeFetcher) Close() error {
	atomic.AddInt32(&f.closeCount, 1)
	return nil
}
func (f *fakeFetcher) FetchNew(_ context.Context) ([]EmailMessage, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queues) == 0 {
		return nil, nil
	}
	msgs := f.queues[0]
	f.queues = f.queues[1:]
	return msgs, nil
}
func (f *fakeFetcher) enqueue(msgs []EmailMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queues = append(f.queues, msgs)
}

func TestEmailToInbound(t *testing.T) {
	cases := []struct {
		name string
		in   EmailMessage
		want InboundMessage
	}{
		{
			name: "subject and body",
			in:   EmailMessage{From: "Alice@Example.com", Subject: "Hello", Body: "Body text"},
			want: InboundMessage{
				SessionID: "email:alice@example.com",
				Channel:   "email",
				Target:    "alice@example.com",
				Message:   "Hello\n\nBody text",
			},
		},
		{
			name: "body only",
			in:   EmailMessage{From: "bob@example.com", Body: "Just a body"},
			want: InboundMessage{
				SessionID: "email:bob@example.com",
				Channel:   "email",
				Target:    "bob@example.com",
				Message:   "Just a body",
			},
		},
		{
			name: "subject only",
			in:   EmailMessage{From: "carol@example.com", Subject: "Subject only"},
			want: InboundMessage{
				SessionID: "email:carol@example.com",
				Channel:   "email",
				Target:    "carol@example.com",
				Message:   "Subject only",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := emailToInbound(tc.in)
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPollerStartDeliversQueuedMessages(t *testing.T) {
	f := &fakeFetcher{}
	f.enqueue([]EmailMessage{
		{From: "x@example.com", Subject: "S1", Body: "B1", UID: 1},
		{From: "y@example.com", Subject: "S2", Body: "B2", UID: 2},
	})

	var got []InboundMessage
	var mu sync.Mutex
	handler := func(_ context.Context, m InboundMessage) error {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := NewEmailInboundPoller(f, 30*time.Second)
	p.Start(ctx, handler, nil)

	// First fetch fires immediately on startup; allow up to 500ms to
	// land before we assert.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	if got[0].SessionID != "email:x@example.com" || got[1].SessionID != "email:y@example.com" {
		t.Fatalf("unexpected session ids: %v / %v", got[0].SessionID, got[1].SessionID)
	}
}

func TestPollerClampsLowInterval(t *testing.T) {
	// Sub-5s intervals are clamped to 30s to protect the IMAP server.
	p := NewEmailInboundPoller(&fakeFetcher{}, 1*time.Second)
	if p.interval != 30*time.Second {
		t.Fatalf("expected 30s clamp, got %v", p.interval)
	}
}

func TestPollerSurfacesHandlerErrors(t *testing.T) {
	f := &fakeFetcher{}
	f.enqueue([]EmailMessage{{From: "a@example.com", Body: "fail me", UID: 99}})

	var seenCh string
	var seenErr error
	cfg := &WebhookInboundConfig{
		OnHandlerError: func(ch string, err error, _ map[string]any) {
			seenCh = ch
			seenErr = err
		},
	}
	failingHandler := func(_ context.Context, _ InboundMessage) error {
		return errors.New("downstream is on fire")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := NewEmailInboundPoller(f, 30*time.Second)
	p.Start(ctx, failingHandler, cfg)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if seenErr != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if seenCh != "email" || seenErr == nil {
		t.Fatalf("expected handler-error observer fired with channel=email; got ch=%q err=%v", seenCh, seenErr)
	}
}

func TestPollerConnectRetriesUntilSuccess(t *testing.T) {
	// Production has a 5s backoff between Connect attempts. Fail ONCE then
	// succeed so the test sits idle for at most one backoff cycle (~5s)
	// rather than retry-twice's 10s — same path coverage, half the wall
	// clock. Both the outer ctx and the inner wait need headroom above 5s.
	var attempts int32
	f := &flakyConnectFetcher{failFirstN: 1, attempts: &attempts}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	p := NewEmailInboundPoller(f, 30*time.Second)
	delivered := make(chan struct{}, 1)
	p.Start(ctx, func(_ context.Context, _ InboundMessage) error {
		select {
		case delivered <- struct{}{}:
		default:
		}
		return nil
	}, nil)

	select {
	case <-delivered:
		// success
	case <-time.After(10 * time.Second):
		t.Fatalf("expected delivery after retry; got %d connect attempts", atomic.LoadInt32(&attempts))
	}
	if got := atomic.LoadInt32(&attempts); got < 2 {
		t.Fatalf("expected ≥2 connect attempts, got %d", got)
	}
}

// flakyConnectFetcher fails Connect the first failFirstN times then succeeds
// and returns one queued message. Used to assert the retry loop works.
type flakyConnectFetcher struct {
	failFirstN int32
	attempts   *int32
	served     bool
}

func (f *flakyConnectFetcher) Connect(_ context.Context) error {
	n := atomic.AddInt32(f.attempts, 1)
	if n <= f.failFirstN {
		return errors.New("simulated connect failure")
	}
	return nil
}
func (f *flakyConnectFetcher) Close() error { return nil }
func (f *flakyConnectFetcher) FetchNew(_ context.Context) ([]EmailMessage, error) {
	if f.served {
		return nil, nil
	}
	f.served = true
	return []EmailMessage{{From: "retry@example.com", Body: "finally", UID: 1}}, nil
}

func TestPollerStopsOnContextCancel(t *testing.T) {
	f := &fakeFetcher{}
	ctx, cancel := context.WithCancel(context.Background())

	p := NewEmailInboundPoller(f, 30*time.Second)
	p.Start(ctx, func(_ context.Context, _ InboundMessage) error { return nil }, nil)

	// Let it start up.
	time.Sleep(100 * time.Millisecond)
	cancel()

	// Give the loop time to notice the cancellation and Close.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&f.closeCount) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Close was not called after ctx cancel; closeCount=%d", atomic.LoadInt32(&f.closeCount))
}

func TestPollerNilHandlerDoesNothing(t *testing.T) {
	// Nil handler is a "disabled" signal — Start must NOT spawn a goroutine.
	f := &fakeFetcher{}
	p := NewEmailInboundPoller(f, 30*time.Second)
	p.Start(context.Background(), nil, nil)
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&f.connectCount) != 0 {
		t.Fatalf("nil handler should not connect; got %d connects", atomic.LoadInt32(&f.connectCount))
	}
}

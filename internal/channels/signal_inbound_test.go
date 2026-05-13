package channels

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSignalFetcher returns scripted slices of messages on each call. Once
// the script is exhausted it blocks on ctx.Done — tests terminate the loop
// via context cancellation rather than letting the poller tight-loop on a
// "script done" error.
type fakeSignalFetcher struct {
	mu     sync.Mutex
	script [][]SignalInboundMessage
	errs   []error // optional per-call errors aligned with script
	calls  int
}

func (f *fakeSignalFetcher) FetchNew(ctx context.Context) ([]SignalInboundMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls >= len(f.script) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	i := f.calls
	f.calls++
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	return f.script[i], err
}

// captureHandler collects every InboundMessage the poller dispatches.
type captureHandler struct {
	mu   sync.Mutex
	msgs []InboundMessage
}

func (c *captureHandler) handle(_ context.Context, m InboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	return nil
}

func (c *captureHandler) snapshot() []InboundMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]InboundMessage, len(c.msgs))
	copy(out, c.msgs)
	return out
}

func TestSignalInboundPollerDispatchesDirectMessage(t *testing.T) {
	fetcher := &fakeSignalFetcher{
		script: [][]SignalInboundMessage{
			{{Source: "+15551234567", Message: "hello bot", Timestamp: 1700000000000}},
		},
	}
	p := NewSignalInboundPoller(fetcher)
	p.errorBackoff = 10 * time.Millisecond // shrink so the script-done error path doesn't slow the test

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cap := &captureHandler{}
	p.Start(ctx, cap.handle, nil)

	waitFor(t, time.Second, func() bool { return len(cap.snapshot()) == 1 })
	cancel()

	msgs := cap.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("want 1 dispatched msg, got %d", len(msgs))
	}
	want := InboundMessage{
		SessionID: "signal:+15551234567",
		Channel:   "signal",
		Target:    "+15551234567",
		Message:   "hello bot",
	}
	if !reflect.DeepEqual(msgs[0], want) {
		t.Fatalf("want %+v\n got %+v", want, msgs[0])
	}
}

func TestSignalInboundPollerThreadsGroupMessages(t *testing.T) {
	// Two participants posting to the same group should land in the same
	// session id, while the same participants' direct messages thread by
	// sender — proves group session id derives from groupId, not source.
	fetcher := &fakeSignalFetcher{
		script: [][]SignalInboundMessage{
			{
				{Source: "+15551111111", Message: "alice in group", GroupID: "GROUP_BASE64="},
				{Source: "+15552222222", Message: "bob in group", GroupID: "GROUP_BASE64="},
				{Source: "+15551111111", Message: "alice direct"},
			},
		},
	}
	p := NewSignalInboundPoller(fetcher)
	p.errorBackoff = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cap := &captureHandler{}
	p.Start(ctx, cap.handle, nil)
	waitFor(t, time.Second, func() bool { return len(cap.snapshot()) == 3 })
	cancel()

	msgs := cap.snapshot()
	if len(msgs) != 3 {
		t.Fatalf("want 3, got %d", len(msgs))
	}
	if msgs[0].SessionID != "signal-group:GROUP_BASE64=" || msgs[0].Target != "GROUP_BASE64=" {
		t.Errorf("first msg should thread by group: %+v", msgs[0])
	}
	if msgs[1].SessionID != "signal-group:GROUP_BASE64=" || msgs[1].Target != "GROUP_BASE64=" {
		t.Errorf("second msg should thread by same group: %+v", msgs[1])
	}
	if msgs[2].SessionID != "signal:+15551111111" || msgs[2].Target != "+15551111111" {
		t.Errorf("third msg should thread by sender: %+v", msgs[2])
	}
}

func TestSignalInboundPollerSkipsEmptyBatches(t *testing.T) {
	// First call returns no messages — that's the normal "long-poll
	// timeout hit, nothing arrived" path. Poller MUST loop and try again.
	fetcher := &fakeSignalFetcher{
		script: [][]SignalInboundMessage{
			{}, // empty
			{{Source: "+1", Message: "second-call msg"}},
		},
	}
	p := NewSignalInboundPoller(fetcher)
	p.errorBackoff = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cap := &captureHandler{}
	p.Start(ctx, cap.handle, nil)
	waitFor(t, time.Second, func() bool { return len(cap.snapshot()) == 1 })
	cancel()

	if got := cap.snapshot(); len(got) != 1 || got[0].Message != "second-call msg" {
		t.Fatalf("expected single msg from second call, got %+v", got)
	}
}

// errorThenSuccessFetcher errors on the first call, then succeeds.
// Verifies the loop applies the error backoff and continues instead of
// dying. Also exercises the "ctx cancelled while in backoff" path
// implicitly when the test ends.
type errorThenSuccessFetcher struct {
	calls atomic.Int32
}

func (f *errorThenSuccessFetcher) FetchNew(ctx context.Context) ([]SignalInboundMessage, error) {
	n := f.calls.Add(1)
	if n == 1 {
		return nil, errors.New("transient")
	}
	if n == 2 {
		return []SignalInboundMessage{{Source: "+15550000000", Message: "after backoff"}}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestSignalInboundPollerRecoversFromFetcherError(t *testing.T) {
	fetcher := &errorThenSuccessFetcher{}
	p := NewSignalInboundPoller(fetcher)
	p.errorBackoff = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cap := &captureHandler{}
	p.Start(ctx, cap.handle, nil)
	waitFor(t, 2*time.Second, func() bool { return len(cap.snapshot()) == 1 })
	cancel()

	if got := cap.snapshot(); len(got) != 1 || got[0].Message != "after backoff" {
		t.Fatalf("expected single msg after recovery, got %+v", got)
	}
}

// raisingHandler always errors so we can verify OnHandlerError fires.
type raisingHandler struct {
	errs atomic.Int32
}

func (r *raisingHandler) handle(_ context.Context, _ InboundMessage) error {
	r.errs.Add(1)
	return errors.New("handler boom")
}

func TestSignalInboundPollerCallsOnHandlerError(t *testing.T) {
	fetcher := &fakeSignalFetcher{
		script: [][]SignalInboundMessage{
			{{Source: "+1", Message: "x", GroupID: "G="}},
		},
	}
	p := NewSignalInboundPoller(fetcher)
	p.errorBackoff = 10 * time.Millisecond

	var seenChannel string
	var seenAttrs map[string]any
	var mu sync.Mutex
	var seenCount atomic.Int32
	cfg := &WebhookInboundConfig{
		OnHandlerError: func(ch string, _ error, attrs map[string]any) {
			mu.Lock()
			seenChannel = ch
			seenAttrs = attrs
			mu.Unlock()
			seenCount.Add(1)
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &raisingHandler{}
	p.Start(ctx, h.handle, cfg)
	waitFor(t, time.Second, func() bool { return seenCount.Load() >= 1 })
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if seenChannel != "signal" {
		t.Errorf("expected channel=signal, got %q", seenChannel)
	}
	if seenAttrs["sessionId"] != "signal-group:G=" {
		t.Errorf("expected sessionId signal-group:G=, got %v", seenAttrs["sessionId"])
	}
	if seenAttrs["from"] != "+1" {
		t.Errorf("expected from=+1, got %v", seenAttrs["from"])
	}
	if seenAttrs["group"] != "G=" {
		t.Errorf("expected group=G=, got %v", seenAttrs["group"])
	}
}

func TestSignalInboundPollerNoOpOnNilFetcherOrHandler(t *testing.T) {
	p := NewSignalInboundPoller(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx, nil, nil) // both nil — must not panic

	p2 := NewSignalInboundPoller(&fakeSignalFetcher{})
	p2.Start(ctx, nil, nil) // nil handler — must not panic
}

func TestSignalToInboundTrimsAllFields(t *testing.T) {
	got := signalToInbound(SignalInboundMessage{
		Source:  "  +15551234567 ",
		Message: "  hello  ",
		GroupID: "  ", // whitespace-only treated as no-group
	})
	want := InboundMessage{
		SessionID: "signal:+15551234567",
		Channel:   "signal",
		Target:    "+15551234567",
		Message:   "hello",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %+v\n got %+v", want, got)
	}
}

// waitFor polls until cond is true or timeout. Test helper.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

package channels

import (
	"context"
	"strings"
	"time"
)

type Router struct {
	items      map[string]Channel
	maxRetries int
}

// NewRouter returns a Router with 3 retries (4 total attempts) on Send failures.
func NewRouter() *Router {
	return &Router{
		items:      map[string]Channel{},
		maxRetries: 3,
	}
}

// NewRouterWithRetries returns a Router with a configurable retry count.
// maxRetries=0 means a single attempt with no retries.
func NewRouterWithRetries(maxRetries int) *Router {
	return &Router{
		items:      map[string]Channel{},
		maxRetries: maxRetries,
	}
}

// SetMaxRetries configures the number of retry attempts after an initial failure.
// Values above 10 are clamped to prevent absurdly long backoff chains.
func (r *Router) SetMaxRetries(n int) {
	if n < 0 {
		n = 0
	}
	if n > 10 {
		n = 10
	}
	r.maxRetries = n
}

func (r *Router) Register(ch Channel) {
	if ch == nil {
		return
	}
	r.items[strings.ToLower(ch.Name())] = ch
}

// Names returns the names of all registered channels.
func (r *Router) Names() []string {
	names := make([]string, 0, len(r.items))
	for k := range r.items {
		names = append(names, k)
	}
	return names
}

// Dispatch sends message to the named channel with exponential-backoff retries.
// It makes up to maxRetries+1 total attempts; backoff starts at 200 ms and
// doubles each retry, capped at 5 s. Returns nil on first success.
func (r *Router) Dispatch(ctx context.Context, message OutboundMessage) error {
	channelName := strings.ToLower(strings.TrimSpace(message.Channel))
	if channelName == "" {
		return nil
	}
	ch, ok := r.items[channelName]
	if !ok {
		return nil
	}
	var lastErr error
	for attempt := 0; attempt <= r.maxRetries; attempt++ {
		if attempt > 0 {
			// Cap the shift to 5 before multiplying to avoid integer overflow.
			shift := uint(attempt - 1)
			if shift > 5 {
				shift = 5
			}
			backoff := time.Duration(200<<shift) * time.Millisecond
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		if err := ch.Send(ctx, message); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

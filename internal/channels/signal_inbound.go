package channels

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// SignalInboundMessage is the narrow representation of a signal-cli envelope
// surfaced by the fetcher. We deliberately don't expose the full envelope
// JSON shape to the rest of the gateway so that the upstream sidecar can
// evolve (or be swapped for signald, dbus-signal-cli, etc.) without rippling
// changes through the channels package.
type SignalInboundMessage struct {
	// Source is the sender's Signal identity — phone number "+15551234567"
	// or UUID depending on what signal-cli-rest-api surfaces.
	Source string
	// Message is the text body. Empty messages (read receipts, typing
	// indicators) are filtered out by the fetcher before we see them.
	Message string
	// GroupID is the base64 group id when the message is a group message;
	// empty for direct messages. Session-id derivation uses it to fan
	// group messages into a per-group conversation.
	GroupID string
	// Timestamp is the Signal message timestamp (ms since epoch). Kept so
	// downstream can correlate with signal-cli logs; not used for dedupe
	// — signal-cli already removes delivered messages from the receive
	// queue.
	Timestamp int64
}

// SignalFetcher is the seam tests use to substitute the real HTTP fetcher.
// Production is SignalHTTPFetcher in signal_inbound_http.go. Implementations
// MUST block for at most their configured timeout and MUST return an empty
// slice (not an error) when no messages arrived.
type SignalFetcher interface {
	// FetchNew long-polls signal-cli for new messages, returning all that
	// arrived within the timeout. Empty slice on no-messages is normal.
	// Errors are treated as transient by the poller — see loop() backoff.
	FetchNew(ctx context.Context) ([]SignalInboundMessage, error)
}

// SignalInboundPoller drives a SignalFetcher in a goroutine and dispatches
// each surfaced message to the supplied handler as a channels.InboundMessage.
// Unlike the email poller (which uses a ticker on top of a synchronous
// fetcher), the signal fetcher is already long-polling — so the loop just
// re-fires on every successful return. A short backoff is inserted only on
// errors so we don't tight-loop against a dead sidecar.
type SignalInboundPoller struct {
	fetcher SignalFetcher
	// errorBackoff controls the delay after a fetcher error before
	// retrying. Defaulted in the constructor; exposed as a field so tests
	// can shrink it without exposing setters.
	errorBackoff time.Duration
	closeOnce    sync.Once
	closed       chan struct{}
}

// NewSignalInboundPoller wires a fetcher with default error backoff (5s).
func NewSignalInboundPoller(fetcher SignalFetcher) *SignalInboundPoller {
	return &SignalInboundPoller{
		fetcher:      fetcher,
		errorBackoff: 5 * time.Second,
		closed:       make(chan struct{}),
	}
}

// Start launches the poll loop. Returns immediately; loop terminates when
// ctx is cancelled. cfg.OnHandlerError receives per-message handler errors.
func (p *SignalInboundPoller) Start(
	ctx context.Context,
	handler func(context.Context, InboundMessage) error,
	cfg *WebhookInboundConfig,
) {
	if p == nil || p.fetcher == nil || handler == nil {
		return
	}
	go p.loop(ctx, handler, cfg)
}

// Close signals the poller to stop on its next iteration. The current
// in-flight FetchNew will only return when its ctx is done; callers wanting
// prompt shutdown should cancel the Start ctx.
func (p *SignalInboundPoller) Close() error {
	p.closeOnce.Do(func() { close(p.closed) })
	return nil
}

func (p *SignalInboundPoller) loop(
	ctx context.Context,
	handler func(context.Context, InboundMessage) error,
	cfg *WebhookInboundConfig,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.closed:
			return
		default:
		}
		msgs, err := p.fetcher.FetchNew(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Fprintf(os.Stderr, "[signal] inbound fetch failed: %v (retrying in %s)\n", err, p.errorBackoff)
			select {
			case <-ctx.Done():
				return
			case <-p.closed:
				return
			case <-time.After(p.errorBackoff):
			}
			continue
		}
		for _, m := range msgs {
			inbound := signalToInbound(m)
			if err := handler(ctx, inbound); err != nil && cfg != nil && cfg.OnHandlerError != nil {
				cfg.OnHandlerError("signal", err, map[string]any{
					"sessionId": inbound.SessionID,
					"from":      m.Source,
					"group":     m.GroupID,
					"timestamp": m.Timestamp,
				})
			}
		}
	}
}

// signalToInbound derives an InboundMessage from a SignalInboundMessage.
// Direct messages thread by sender ("signal:+15551234567"); group messages
// thread by group id ("signal-group:<base64>") so every participant lands
// in the same conversation. Target carries the same identifier so outbound
// replies route back: signal-cli's /v2/send accepts both numbers and group
// ids in the `recipients` field.
func signalToInbound(m SignalInboundMessage) InboundMessage {
	source := strings.TrimSpace(m.Source)
	group := strings.TrimSpace(m.GroupID)
	var sid, target string
	if group != "" {
		sid = "signal-group:" + group
		target = group
	} else {
		sid = "signal:" + source
		target = source
	}
	return InboundMessage{
		SessionID: sid,
		Channel:   "signal",
		Target:    target,
		Message:   strings.TrimSpace(m.Message),
	}
}

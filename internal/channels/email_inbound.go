package channels

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// EmailMessage is the narrow representation of an inbound email that the
// poller surfaces to the rest of the gateway. We deliberately do NOT pass
// emersion/go-imap types up the stack so a future provider switch (Gmail
// API, Exchange Web Services, IMAP4rev2, etc.) doesn't require touching
// every call site.
type EmailMessage struct {
	From    string // RFC 5322 sender address (without the display name)
	Subject string
	Body    string // text/plain body if available; falls back to the first text part
	UID     uint32 // IMAP UID, for ack/state tracking
}

// EmailFetcher is the seam tests use to substitute an in-memory mailbox.
// The production implementation in imap_fetcher.go wraps emersion's
// imapclient and talks real IMAP. Tests can implement this in ~20 lines.
type EmailFetcher interface {
	// Connect dials the upstream and authenticates. Idempotent — repeat
	// calls after a successful one should be no-ops.
	Connect(ctx context.Context) error
	// Close releases the connection. Safe to call after a failed Connect.
	Close() error
	// FetchNew returns messages that haven't been delivered to us before
	// AND marks them seen on the upstream. The poller treats a non-nil
	// error as "transient, retry on next tick" and never crashes.
	FetchNew(ctx context.Context) ([]EmailMessage, error)
}

// EmailInboundPoller fetches unseen messages from an EmailFetcher on a
// fixed interval and dispatches each to the supplied handler as an
// InboundMessage. The session id is derived from the From address so
// multiple messages from the same sender land in the same conversation.
//
// The poller is deliberately interval-based (not IMAP IDLE) for v1 —
// email delivery has minute-scale expectations anyway and the simpler
// loop is easier to reason about / reconnect after transient failures.
type EmailInboundPoller struct {
	fetcher  EmailFetcher
	interval time.Duration
	// closeOnce guards repeated Close() calls so the fetcher's Close is
	// only ever invoked once. Without this an aggressive shutdown caller
	// could double-close the underlying TCP connection.
	closeOnce sync.Once
}

// NewEmailInboundPoller wires a fetcher + poll interval. Interval values
// under 5 seconds are clamped up because we don't want operators
// accidentally hammering their IMAP provider.
func NewEmailInboundPoller(fetcher EmailFetcher, interval time.Duration) *EmailInboundPoller {
	if interval < 5*time.Second {
		interval = 30 * time.Second
	}
	return &EmailInboundPoller{fetcher: fetcher, interval: interval}
}

// Start launches the poll loop. Returns immediately; the loop terminates
// when ctx is cancelled or Close is called. cfg.OnHandlerError, if set,
// receives per-message handler errors with channel="email".
func (p *EmailInboundPoller) Start(
	ctx context.Context,
	handler func(context.Context, InboundMessage) error,
	cfg *WebhookInboundConfig,
) {
	if p == nil || handler == nil {
		return
	}
	go p.loop(ctx, handler, cfg)
}

func (p *EmailInboundPoller) Close() error {
	var err error
	p.closeOnce.Do(func() {
		if p.fetcher != nil {
			err = p.fetcher.Close()
		}
	})
	return err
}

// loop is the main polling loop. Connect failures retry with a 5-second
// backoff (not exponential — IMAP servers don't reward slow retries).
// Per-tick fetch failures also retry on the next tick without backoff.
func (p *EmailInboundPoller) loop(
	ctx context.Context,
	handler func(context.Context, InboundMessage) error,
	cfg *WebhookInboundConfig,
) {
	defer p.Close()

	// First connect — keep trying until ctx is done or it succeeds.
	for {
		if err := p.fetcher.Connect(ctx); err == nil {
			break
		} else {
			fmt.Fprintf(os.Stderr, "[email] inbound connect failed: %v (retrying in 5s)\n", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	// Fire one fetch immediately on startup so we don't miss messages
	// that arrived during gateway downtime.
	p.fetchAndDispatch(ctx, handler, cfg)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.fetchAndDispatch(ctx, handler, cfg)
		}
	}
}

func (p *EmailInboundPoller) fetchAndDispatch(
	ctx context.Context,
	handler func(context.Context, InboundMessage) error,
	cfg *WebhookInboundConfig,
) {
	msgs, err := p.fetcher.FetchNew(ctx)
	if err != nil {
		// Transient — log and let the next tick retry. Connection
		// errors typically surface here; the imapFetcher attempts
		// reconnect internally on the next call.
		fmt.Fprintf(os.Stderr, "[email] inbound fetch failed: %v\n", err)
		return
	}
	for _, m := range msgs {
		inbound := emailToInbound(m)
		if err := handler(ctx, inbound); err != nil && cfg != nil && cfg.OnHandlerError != nil {
			cfg.OnHandlerError("email", err, map[string]any{
				"sessionId": inbound.SessionID,
				"from":      m.From,
				"subject":   m.Subject,
				"uid":       m.UID,
			})
		}
	}
}

// emailToInbound derives an InboundMessage from an EmailMessage. SessionID
// is `email:<from-address>` so all messages from the same sender thread
// into a single conversation — same posture as Telegram's per-chat id.
// Subject + body are concatenated with a newline so the agent sees both.
func emailToInbound(m EmailMessage) InboundMessage {
	from := strings.TrimSpace(strings.ToLower(m.From))
	body := strings.TrimSpace(m.Body)
	subject := strings.TrimSpace(m.Subject)
	var msg string
	switch {
	case subject != "" && body != "":
		msg = subject + "\n\n" + body
	case body != "":
		msg = body
	default:
		msg = subject
	}
	return InboundMessage{
		SessionID: "email:" + from,
		Channel:   "email",
		Target:    from, // the sender — outbound replies go back to them
		Message:   msg,
	}
}

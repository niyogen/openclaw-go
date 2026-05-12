package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SignalChannel sends outbound messages via the signal-cli-rest-api sidecar
// (https://github.com/bbernhard/signal-cli-rest-api). Operators run that
// sidecar locally and we POST to its `/v2/send` endpoint.
//
// Inbound is deferred: signal-cli surfaces messages via long-polling
// `/v1/receive/{number}` or an SSE stream at `/api/v1/events`. Both work,
// but adding a poller increases the channel's runtime footprint
// significantly; users who need inbound today should pair Signal-out with
// another inbound channel (Telegram/Slack) or open an issue.
type SignalChannel struct {
	baseURL string // e.g. "http://127.0.0.1:8080"
	number  string // bot's own Signal number, "+15551234567"
	client  *http.Client
}

// NewSignalChannel constructs the channel. Empty baseURL OR number disables
// Send (returns nil) — matches the disabled-channel behaviour of the other
// implementations.
func NewSignalChannel(baseURL, number string) *SignalChannel {
	return &SignalChannel{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		number:  strings.TrimSpace(number),
		client: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (s *SignalChannel) Name() string {
	return "signal"
}

// Send posts a single text message. Target can be a single recipient
// ("+15551231212") or a Signal group ID — signal-cli's `recipients` field
// accepts either, so we forward the user's value as-is.
func (s *SignalChannel) Send(ctx context.Context, message OutboundMessage) error {
	if s.baseURL == "" || s.number == "" {
		return nil // disabled — see constructor
	}
	target := strings.TrimSpace(message.Target)
	if target == "" {
		return fmt.Errorf("signal: target is required")
	}
	payload := map[string]any{
		"message":    message.Message,
		"number":     s.number,
		"recipients": []string{target},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := s.baseURL + "/v2/send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("signal: POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("signal: %s returned %d: %s", url, resp.StatusCode, string(body))
	}
	return nil
}

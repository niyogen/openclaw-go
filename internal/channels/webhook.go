package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type WebhookChannel struct {
	outboundURL string
	client      *http.Client
}

func NewWebhookChannel(outboundURL string) *WebhookChannel {
	return &WebhookChannel{
		outboundURL: strings.TrimSpace(outboundURL),
		client: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (w *WebhookChannel) Name() string {
	return "webhook"
}

func (w *WebhookChannel) Send(ctx context.Context, message OutboundMessage) error {
	if w.outboundURL == "" {
		return nil
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.outboundURL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

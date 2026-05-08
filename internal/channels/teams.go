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

type TeamsChannel struct {
	outboundURL string
	client      *http.Client
}

func NewTeamsChannel(outboundURL string) *TeamsChannel {
	return &TeamsChannel{
		outboundURL: strings.TrimSpace(outboundURL),
		client: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (t *TeamsChannel) Name() string {
	return "teams"
}

func (t *TeamsChannel) Send(ctx context.Context, message OutboundMessage) error {
	if t.outboundURL == "" {
		return nil
	}
	payload := map[string]any{
		"text": message.Message,
		"type": "message",
	}
	if strings.TrimSpace(message.ThreadID) != "" {
		payload["replyToId"] = message.ThreadID
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.outboundURL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("teams outbound webhook returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func BuildTeamsWebhookHandler(
	webhookSecret string,
	handler func(context.Context, InboundMessage) error,
) http.HandlerFunc {
	secret := strings.TrimSpace(webhookSecret)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if secret != "" {
			headerSecret := strings.TrimSpace(r.Header.Get("X-OpenClaw-Webhook-Token"))
			if headerSecret == "" || headerSecret != secret {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		inboundMessages, err := decodeTeamsWebhook(body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, inbound := range inboundMessages {
			_ = handler(r.Context(), inbound)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

type teamsWebhookPayload struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	Conversation struct {
		ID string `json:"id"`
	} `json:"conversation"`
	From struct {
		ID string `json:"id"`
	} `json:"from"`
}

func decodeTeamsWebhook(raw []byte) ([]InboundMessage, error) {
	var payload teamsWebhookPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	activityType := strings.ToLower(strings.TrimSpace(payload.Type))
	if activityType != "" && activityType != "message" {
		return nil, nil
	}
	text := strings.TrimSpace(payload.Text)
	conversationID := strings.TrimSpace(payload.Conversation.ID)
	if text == "" || conversationID == "" {
		return nil, nil
	}
	sessionID := "teams:" + conversationID
	if strings.TrimSpace(payload.From.ID) != "" {
		sessionID += ":" + strings.TrimSpace(payload.From.ID)
	}
	return []InboundMessage{
		{
			SessionID: sessionID,
			Channel:   "teams",
			Target:    conversationID,
			Message:   text,
		},
	}, nil
}

package channels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LineChannel sends messages via the LINE Messaging API (reply + push).
type LineChannel struct {
	channelToken  string
	channelSecret string
	client        *http.Client
}

func NewLineChannel(channelToken, channelSecret string) *LineChannel {
	return &LineChannel{
		channelToken:  strings.TrimSpace(channelToken),
		channelSecret: strings.TrimSpace(channelSecret),
		client:        &http.Client{Timeout: 20 * time.Second},
	}
}

func (l *LineChannel) Name() string { return "line" }

func (l *LineChannel) Send(ctx context.Context, message OutboundMessage) error {
	if l.channelToken == "" {
		return nil
	}
	target := strings.TrimSpace(message.Target)
	if target == "" {
		return nil
	}
	payload := map[string]any{
		"to": target,
		"messages": []map[string]string{
			{"type": "text", "text": message.Message},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.line.me/v2/bot/message/push", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.channelToken)
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("line push returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// BuildLineWebhookHandler handles LINE webhook events with signature verification.
func BuildLineWebhookHandler(
	channelSecret string,
	handler func(context.Context, InboundMessage) error,
) http.HandlerFunc {
	secret := strings.TrimSpace(channelSecret)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := readWebhookBody(w, r)
		if err != nil {
			if errBodyTooLarge(err) {
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if secret != "" && !verifyLineSignature(secret, r, body) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		messages, err := decodeLineWebhook(body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, msg := range messages {
			_ = handler(r.Context(), msg)
		}
		w.WriteHeader(http.StatusOK)
	}
}

func verifyLineSignature(secret string, r *http.Request, body []byte) bool {
	sig := strings.TrimSpace(r.Header.Get("X-Line-Signature"))
	if sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sig))
}

type lineWebhookPayload struct {
	Events []struct {
		Type    string `json:"type"`
		ReplyTo string `json:"replyToken"`
		Source  struct {
			Type   string `json:"type"`
			UserID string `json:"userId"`
		} `json:"source"`
		Message struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"message"`
	} `json:"events"`
}

func decodeLineWebhook(raw []byte) ([]InboundMessage, error) {
	var payload lineWebhookPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	var out []InboundMessage
	for _, ev := range payload.Events {
		if ev.Type != "message" || ev.Message.Type != "text" {
			continue
		}
		text := strings.TrimSpace(ev.Message.Text)
		userID := strings.TrimSpace(ev.Source.UserID)
		if text == "" || userID == "" {
			continue
		}
		out = append(out, InboundMessage{
			SessionID: "line:" + userID,
			Channel:   "line",
			Target:    userID,
			Message:   text,
		})
	}
	return out, nil
}

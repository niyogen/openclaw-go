package channels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type SlackChannel struct {
	botToken  string
	channelID string
	client    *http.Client
}

func NewSlackChannel(botToken, channelID string) *SlackChannel {
	return &SlackChannel{
		botToken:  strings.TrimSpace(botToken),
		channelID: strings.TrimSpace(channelID),
		client: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (s *SlackChannel) Name() string {
	return "slack"
}

func (s *SlackChannel) Send(ctx context.Context, message OutboundMessage) error {
	if s.botToken == "" {
		return nil
	}
	targetChannel := strings.TrimSpace(message.Target)
	if targetChannel == "" {
		targetChannel = s.channelID
	}
	if targetChannel == "" {
		return nil
	}
	payload := map[string]any{
		"channel": targetChannel,
		"text":    message.Message,
	}
	if strings.TrimSpace(message.ThreadID) != "" {
		payload["thread_ts"] = message.ThreadID
	}
	if strings.TrimSpace(message.MediaURL) != "" {
		payload["attachments"] = []map[string]string{{"image_url": message.MediaURL}}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://slack.com/api/chat.postMessage",
		bytes.NewReader(raw),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.botToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("slack chat.postMessage returned %d", resp.StatusCode)
	}
	return nil
}

func BuildSlackWebhookHandler(
	signingSecret string,
	handler func(context.Context, InboundMessage) error,
) http.HandlerFunc {
	secret := strings.TrimSpace(signingSecret)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if secret != "" && !verifySlackSignature(secret, r, body) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		replyBody, inboundMessages, err := decodeSlackEvents(body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, inbound := range inboundMessages {
			_ = handler(r.Context(), inbound)
		}
		w.Header().Set("Content-Type", "application/json")
		if replyBody == "" {
			replyBody = `{"ok":true}`
		}
		_, _ = w.Write([]byte(replyBody))
	}
}

func verifySlackSignature(secret string, r *http.Request, body []byte) bool {
	signature := strings.TrimSpace(r.Header.Get("X-Slack-Signature"))
	timestamp := strings.TrimSpace(r.Header.Get("X-Slack-Request-Timestamp"))
	if signature == "" || timestamp == "" {
		return false
	}
	if tsInt, err := strconv.ParseInt(timestamp, 10, 64); err == nil {
		now := time.Now().Unix()
		diff := now - tsInt
		if diff < 0 {
			diff = -diff
		}
		if diff > 60*5 {
			return false
		}
	}
	base := "v0:" + timestamp + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(base))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

type slackEnvelope struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Event     struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Text    string `json:"text"`
		Channel string `json:"channel"`
		BotID   string `json:"bot_id"`
		User    string `json:"user"`
	} `json:"event"`
}

func decodeSlackEvents(raw []byte) (string, []InboundMessage, error) {
	var env slackEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", nil, err
	}
	switch env.Type {
	case "url_verification":
		return fmt.Sprintf(`{"challenge":"%s"}`, env.Challenge), nil, nil
	case "event_callback":
		if env.Event.Type != "message" || env.Event.BotID != "" || env.Event.Subtype != "" {
			return `{"ok":true}`, nil, nil
		}
		text := strings.TrimSpace(env.Event.Text)
		channel := strings.TrimSpace(env.Event.Channel)
		if text == "" || channel == "" {
			return `{"ok":true}`, nil, nil
		}
		sessionID := "slack:" + channel
		if strings.TrimSpace(env.Event.User) != "" {
			sessionID += ":" + strings.TrimSpace(env.Event.User)
		}
		return `{"ok":true}`, []InboundMessage{
			{
				SessionID: sessionID,
				Channel:   "slack",
				Target:    channel,
				Message:   text,
			},
		}, nil
	default:
		return `{"ok":true}`, nil, nil
	}
}

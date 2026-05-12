package channels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type DiscordChannel struct {
	botToken  string
	channelID string
	client    *http.Client
}

func NewDiscordChannel(botToken, channelID string) *DiscordChannel {
	return &DiscordChannel{
		botToken:  strings.TrimSpace(botToken),
		channelID: strings.TrimSpace(channelID),
		client: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (d *DiscordChannel) Name() string {
	return "discord"
}

func (d *DiscordChannel) Send(ctx context.Context, message OutboundMessage) error {
	if d.botToken == "" {
		return nil
	}
	targetChannel := strings.TrimSpace(message.Target)
	if targetChannel == "" {
		targetChannel = d.channelID
	}
	if targetChannel == "" {
		return nil
	}
	payload := map[string]string{
		"content": message.Message,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages", targetChannel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bot "+d.botToken)
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discord send returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func BuildDiscordWebhookHandler(
	webhookToken string,
	handler func(context.Context, InboundMessage) error,
) http.HandlerFunc {
	secret := strings.TrimSpace(webhookToken)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if secret != "" {
			header := strings.TrimSpace(r.Header.Get("X-OpenClaw-Webhook-Token"))
			if header == "" || !hmac.Equal([]byte(header), []byte(secret)) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		body, err := readWebhookBody(w, r)
		if err != nil {
			if errBodyTooLarge(err) {
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		inboundMessages, err := decodeDiscordWebhook(body)
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

type discordWebhookPayload struct {
	Content   string `json:"content"`
	ChannelID string `json:"channel_id"`
	Author    struct {
		Bot bool `json:"bot"`
		ID  string
	} `json:"author"`
}

func decodeDiscordWebhook(raw []byte) ([]InboundMessage, error) {
	var payload discordWebhookPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Author.Bot {
		return nil, nil
	}
	content := strings.TrimSpace(payload.Content)
	channelID := strings.TrimSpace(payload.ChannelID)
	if content == "" || channelID == "" {
		return nil, nil
	}
	sessionID := "discord:" + channelID
	if strings.TrimSpace(payload.Author.ID) != "" {
		sessionID += ":" + strings.TrimSpace(payload.Author.ID)
	}
	return []InboundMessage{
		{
			SessionID: sessionID,
			Channel:   "discord",
			Target:    channelID,
			Message:   content,
		},
	}, nil
}

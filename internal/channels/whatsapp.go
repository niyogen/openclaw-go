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
	"strings"
	"time"
)

type WhatsAppChannel struct {
	accessToken   string
	phoneNumberID string
	toNumber      string
	client        *http.Client
}

func NewWhatsAppChannel(accessToken, phoneNumberID, toNumber string) *WhatsAppChannel {
	return &WhatsAppChannel{
		accessToken:   strings.TrimSpace(accessToken),
		phoneNumberID: strings.TrimSpace(phoneNumberID),
		toNumber:      strings.TrimSpace(toNumber),
		client: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (w *WhatsAppChannel) Name() string {
	return "whatsapp"
}

func (w *WhatsAppChannel) Send(ctx context.Context, message OutboundMessage) error {
	if w.accessToken == "" || w.phoneNumberID == "" {
		return nil
	}
	to := strings.TrimSpace(message.Target)
	if to == "" {
		to = w.toNumber
	}
	if to == "" {
		return nil
	}
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "text",
		"text": map[string]string{
			"body": message.Message,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://graph.facebook.com/v20.0/%s/messages", w.phoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.accessToken)
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("whatsapp outbound returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func BuildWhatsAppWebhookHandler(
	verifyToken string,
	appSecret string,
	handler func(context.Context, InboundMessage) error,
	cfg *WebhookInboundConfig,
) http.HandlerFunc {
	expectedVerifyToken := strings.TrimSpace(verifyToken)
	trimmedAppSecret := strings.TrimSpace(appSecret)
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			mode := strings.TrimSpace(r.URL.Query().Get("hub.mode"))
			token := strings.TrimSpace(r.URL.Query().Get("hub.verify_token"))
			challenge := strings.TrimSpace(r.URL.Query().Get("hub.challenge"))
			// Require a configured verify token so hub.mode=subscribe cannot be satisfied blindly.
			if expectedVerifyToken == "" || mode != "subscribe" || challenge == "" || token != expectedVerifyToken {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(challenge))
			return
		case http.MethodPost:
			body, err := readWebhookBody(w, r)
			if err != nil {
				if errBodyTooLarge(err) {
					return
				}
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if trimmedAppSecret != "" && !verifyWhatsAppSignature(trimmedAppSecret, r, body) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			inboundMessages, err := decodeWhatsAppWebhook(body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, inbound := range inboundMessages {
				if err := handler(r.Context(), inbound); err != nil && cfg != nil && cfg.OnHandlerError != nil {
					cfg.OnHandlerError("whatsapp", err, map[string]any{
						"sessionId": inbound.SessionID,
						"target":    inbound.Target,
					})
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
	}
}

func verifyWhatsAppSignature(appSecret string, r *http.Request, body []byte) bool {
	signature := strings.TrimSpace(r.Header.Get("X-Hub-Signature-256"))
	if signature == "" {
		return false
	}
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	provided := strings.TrimPrefix(signature, prefix)
	mac := hmac.New(sha256.New, []byte(appSecret))
	_, _ = mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(provided))
}

type whatsAppEnvelope struct {
	Entry []struct {
		Changes []struct {
			Value struct {
				Contacts []struct {
					WaID string `json:"wa_id"`
				} `json:"contacts"`
				Messages []struct {
					Type string `json:"type"`
					From string `json:"from"`
					Text struct {
						Body string `json:"body"`
					} `json:"text"`
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

func decodeWhatsAppWebhook(raw []byte) ([]InboundMessage, error) {
	var env whatsAppEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	result := []InboundMessage{}
	for _, entry := range env.Entry {
		for _, change := range entry.Changes {
			for _, msg := range change.Value.Messages {
				if strings.ToLower(strings.TrimSpace(msg.Type)) != "text" {
					continue
				}
				text := strings.TrimSpace(msg.Text.Body)
				from := strings.TrimSpace(msg.From)
				if text == "" || from == "" {
					continue
				}
				result = append(result, InboundMessage{
					SessionID: "whatsapp:" + from,
					Channel:   "whatsapp",
					Target:    from,
					Message:   text,
				})
			}
		}
	}
	return result, nil
}

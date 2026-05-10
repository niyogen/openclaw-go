package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type TelegramChannel struct {
	botToken string
	chatID   string
	client   *http.Client
}

type telegramAPIResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

func NewTelegramChannel(botToken, chatID string) *TelegramChannel {
	return &TelegramChannel{
		botToken: strings.TrimSpace(botToken),
		chatID:   strings.TrimSpace(chatID),
		client: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (t *TelegramChannel) Name() string {
	return "telegram"
}

func (t *TelegramChannel) Send(ctx context.Context, message OutboundMessage) error {
	if t.botToken == "" {
		return nil
	}
	targetChatID := strings.TrimSpace(message.Target)
	if targetChatID == "" {
		targetChatID = t.chatID
	}
	if targetChatID == "" {
		return nil
	}

	payload := map[string]string{
		"chat_id": targetChatID,
		"text":    message.Message,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
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
		return fmt.Errorf("telegram sendMessage returned %d", resp.StatusCode)
	}
	return nil
}

// answerCallbackQuery dismisses the loading indicator on an inline keyboard button press.
func (t *TelegramChannel) answerCallbackQuery(ctx context.Context, callbackQueryID string) error {
	return telegramAnswerCallbackQuery(ctx, t.client, t.botToken, callbackQueryID)
}

// BuildWebhookHandler returns an http.HandlerFunc that validates the secret token,
// decodes incoming Telegram updates (including callback_query), calls handler for
// each inbound message, and answers callback queries to dismiss loading indicators.
func (t *TelegramChannel) BuildWebhookHandler(
	secret string,
	handler func(context.Context, InboundMessage) error,
) http.HandlerFunc {
	trimmedSecret := strings.TrimSpace(secret)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if trimmedSecret != "" {
			headerSecret := strings.TrimSpace(r.Header.Get("X-Telegram-Bot-Api-Secret-Token"))
			if headerSecret == "" || headerSecret != trimmedSecret {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var update telegramUpdate
		if err := json.Unmarshal(body, &update); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Answer callback query to dismiss the loading indicator before dispatching.
		if update.CallbackQuery != nil && update.CallbackQuery.ID != "" {
			_ = t.answerCallbackQuery(r.Context(), update.CallbackQuery.ID)
		}
		for _, inbound := range messagesFromUpdate(update) {
			_ = handler(r.Context(), inbound)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

func SetTelegramWebhook(
	ctx context.Context,
	botToken string,
	webhookURL string,
	secret string,
) error {
	token := strings.TrimSpace(botToken)
	targetURL := strings.TrimSpace(webhookURL)
	if token == "" || targetURL == "" {
		return fmt.Errorf("bot token and webhook URL are required")
	}
	payload := map[string]any{
		"url": targetURL,
	}
	if strings.TrimSpace(secret) != "" {
		payload["secret_token"] = secret
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("https://api.telegram.org/bot%s/setWebhook", token),
		bytes.NewReader(raw),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("telegram setWebhook returned %d: %s", resp.StatusCode, string(body))
	}
	var decoded telegramAPIResponse
	if err := json.Unmarshal(body, &decoded); err == nil && !decoded.OK {
		return fmt.Errorf("telegram setWebhook failed: %s", decoded.Description)
	}
	return nil
}

// BuildTelegramWebhookHandler is a standalone webhook handler that does not answer
// callback queries (no TelegramChannel instance available). For callback query
// acknowledgement, use TelegramChannel.BuildWebhookHandler instead.
func BuildTelegramWebhookHandler(
	secret string,
	handler func(context.Context, InboundMessage) error,
) http.HandlerFunc {
	trimmedSecret := strings.TrimSpace(secret)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if trimmedSecret != "" {
			headerSecret := strings.TrimSpace(r.Header.Get("X-Telegram-Bot-Api-Secret-Token"))
			if headerSecret == "" || headerSecret != trimmedSecret {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		inboundMessages, err := decodeTelegramUpdates(body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, inbound := range inboundMessages {
			_ = handler(r.Context(), inbound)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

type TelegramPoller struct {
	botToken string
	client   *http.Client
	offset   int64
}

func NewTelegramPoller(botToken string) *TelegramPoller {
	return &TelegramPoller{
		botToken: strings.TrimSpace(botToken),
		client: &http.Client{
			Timeout: 40 * time.Second,
		},
	}
}

func (p *TelegramPoller) Start(
	ctx context.Context,
	handler func(context.Context, InboundMessage) error,
) {
	if p.botToken == "" || handler == nil {
		return
	}
	go p.loop(ctx, handler)
}

type telegramUpdateResponse struct {
	OK     bool             `json:"ok"`
	Result []telegramUpdate `json:"result"`
}

// telegramUser represents a Telegram user or bot in From fields.
type telegramUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsBot    bool   `json:"is_bot"`
}

type telegramUpdate struct {
	UpdateID      int64                  `json:"update_id"`
	Message       *telegramIncoming      `json:"message"`
	CallbackQuery *telegramCallbackQuery `json:"callback_query"`
}

// telegramCallbackQuery is fired when a user presses an inline keyboard button.
type telegramCallbackQuery struct {
	ID      string           `json:"id"`
	From    telegramUser     `json:"from"`
	Message *telegramIncoming `json:"message"`
	Data    string           `json:"data"` // button payload
}

type telegramIncoming struct {
	Text string       `json:"text"`
	From telegramUser `json:"from"`
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

// messagesFromUpdate converts a single telegramUpdate into zero or more InboundMessages.
func messagesFromUpdate(update telegramUpdate) []InboundMessage {
	var result []InboundMessage

	if update.Message != nil && !update.Message.From.IsBot {
		text := strings.TrimSpace(update.Message.Text)
		if text != "" {
			chatID := strconv.FormatInt(update.Message.Chat.ID, 10)
			result = append(result, InboundMessage{
				SessionID: "telegram:" + chatID,
				Channel:   "telegram",
				Target:    chatID,
				Message:   text,
			})
		}
	}

	if update.CallbackQuery != nil {
		data := strings.TrimSpace(update.CallbackQuery.Data)
		if data != "" {
			var chatID string
			if update.CallbackQuery.Message != nil {
				chatID = strconv.FormatInt(update.CallbackQuery.Message.Chat.ID, 10)
			} else {
				chatID = strconv.FormatInt(update.CallbackQuery.From.ID, 10)
			}
			result = append(result, InboundMessage{
				SessionID: "telegram:" + chatID,
				Channel:   "telegram",
				Target:    chatID,
				Message:   data,
			})
		}
	}

	return result
}

func decodeTelegramUpdates(raw []byte) ([]InboundMessage, error) {
	var update telegramUpdate
	if err := json.Unmarshal(raw, &update); err != nil {
		return nil, err
	}
	return messagesFromUpdate(update), nil
}

func (p *TelegramPoller) loop(
	ctx context.Context,
	handler func(context.Context, InboundMessage) error,
) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := p.pollOnce(ctx, handler); err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (p *TelegramPoller) pollOnce(
	ctx context.Context,
	handler func(context.Context, InboundMessage) error,
) error {
	url := fmt.Sprintf(
		"https://api.telegram.org/bot%s/getUpdates?timeout=25&offset=%d",
		p.botToken,
		p.offset,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("telegram getUpdates returned %d: %s", resp.StatusCode, string(body))
	}

	var decoded telegramUpdateResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return err
	}
	if !decoded.OK {
		return fmt.Errorf("telegram getUpdates returned ok=false")
	}

	for _, item := range decoded.Result {
		if item.UpdateID >= p.offset {
			p.offset = item.UpdateID + 1
		}
		// Answer callback queries immediately to dismiss loading indicators.
		if item.CallbackQuery != nil && item.CallbackQuery.ID != "" {
			_ = telegramAnswerCallbackQuery(ctx, p.client, p.botToken, item.CallbackQuery.ID)
		}
		for _, inbound := range messagesFromUpdate(item) {
			_ = handler(ctx, inbound)
		}
	}
	return nil
}

// telegramAnswerCallbackQuery calls the Telegram answerCallbackQuery API to dismiss
// the loading indicator shown when a user taps an inline keyboard button.
func telegramAnswerCallbackQuery(ctx context.Context, client *http.Client, botToken, callbackQueryID string) error {
	payload := map[string]string{"callback_query_id": callbackQueryID}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("telegram answerCallbackQuery returned %d", resp.StatusCode)
	}
	return nil
}

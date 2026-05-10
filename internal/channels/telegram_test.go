package channels

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTelegramBuildWebhookHandler_OnHandlerError(t *testing.T) {
	tg := NewTelegramChannel("dummy-token", "")
	var sawCh string
	var sawErr error
	cfg := &WebhookInboundConfig{
		OnHandlerError: func(ch string, err error, attrs map[string]any) {
			sawCh = ch
			sawErr = err
		},
	}
	h := tg.BuildWebhookHandler("", func(context.Context, InboundMessage) error {
		return errors.New("tg inbound failed")
	}, cfg)

	rec := httptest.NewRecorder()
	body := `{"update_id":1,"message":{"text":"hi","from":{"is_bot":false},"chat":{"id":42}}}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	if sawErr == nil || sawErr.Error() != "tg inbound failed" || sawCh != "telegram" {
		t.Fatalf("observer ch=%q err=%v", sawCh, sawErr)
	}
}

func TestMessagesFromUpdate_CallbackEmptyData(t *testing.T) {
	raw := []byte(`{
		"update_id": 1,
		"callback_query": {
			"id": "cb1",
			"from": {"id": 99, "is_bot": false},
			"message": {"text": "x", "from": {"id": 1, "is_bot": false}, "chat": {"id": 555}}
		}
	}`)
	var u telegramUpdate
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatal(err)
	}
	msgs := messagesFromUpdate(u)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 inbound, got %d", len(msgs))
	}
	if msgs[0].Message != "[callback]" {
		t.Fatalf("message: %q", msgs[0].Message)
	}
	if msgs[0].Target != "555" {
		t.Fatalf("target: %q", msgs[0].Target)
	}
}

func TestMessagesFromUpdate_CallbackWithData(t *testing.T) {
	u := telegramUpdate{
		CallbackQuery: &telegramCallbackQuery{
			ID:   "x",
			Data: "  pick:A  ",
			Message: &telegramIncoming{
				Chat: struct {
					ID int64 `json:"id"`
				}{ID: 42},
			},
		},
	}
	msgs := messagesFromUpdate(u)
	if len(msgs) != 1 || msgs[0].Message != "pick:A" {
		t.Fatalf("got %+v", msgs)
	}
}

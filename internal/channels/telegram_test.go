package channels

import (
	"encoding/json"
	"testing"
)

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

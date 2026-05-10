package channels

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadWebhookBody_MaxBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(strings.NewReader(strings.Repeat("a", int(MaxWebhookBodyBytes)+1024))))
	_, err := readWebhookBody(rec, req)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errBodyTooLarge(err) {
		t.Fatalf("want body-too-large error, got %v", err)
	}
}

func TestBuildWhatsAppWebhookHandler_OnHandlerError(t *testing.T) {
	var sawCh string
	var sawErr error
	cfg := &WebhookInboundConfig{
		OnHandlerError: func(ch string, err error, attrs map[string]any) {
			sawCh = ch
			sawErr = err
			if attrs["sessionId"] != "whatsapp:1" {
				t.Errorf("attrs sessionId = %v", attrs["sessionId"])
			}
		},
	}
	h := BuildWhatsAppWebhookHandler("vt", "", func(context.Context, InboundMessage) error {
		return errors.New("inbound failed")
	}, cfg)

	rec := httptest.NewRecorder()
	payload := `{"entry":[{"changes":[{"value":{"messages":[{"type":"text","from":"1","text":{"body":"hi"}}]}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	if sawErr == nil || sawErr.Error() != "inbound failed" || sawCh != "whatsapp" {
		t.Fatalf("observer ch=%q err=%v", sawCh, sawErr)
	}
}

func TestWhatsAppWebhook_GET_RequiresVerifyTokenMatch(t *testing.T) {
	h := BuildWhatsAppWebhookHandler("secret-verify", "", func(context.Context, InboundMessage) error { return nil }, nil)

	t.Run("reject wrong token", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/wa?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=xyz", nil)
		h(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("code %d", rec.Code)
		}
	})

	t.Run("accept matching token", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/wa?hub.mode=subscribe&hub.verify_token=secret-verify&hub.challenge=xyz", nil)
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code %d", rec.Code)
		}
		if rec.Body.String() != "xyz" {
			t.Fatalf("body %q", rec.Body.String())
		}
	})
}

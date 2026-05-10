package channels

import (
	"context"
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

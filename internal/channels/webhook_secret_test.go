package channels

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests pin the behaviour of the constant-time secret comparison added
// to discord/teams/telegram webhook handlers: missing or wrong header is 401,
// matching header is 2xx, and an unset configured secret skips the check.

func TestDiscordWebhookSecret(t *testing.T) {
	const secret = "s3kr1t"
	h := BuildDiscordWebhookHandler(secret, func(context.Context, InboundMessage) error { return nil })
	body := `{"content":"hi","channel_id":"c1","author":{"bot":false,"id":"u1"}}`

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"missing header", "", http.StatusUnauthorized},
		{"wrong secret", "nope", http.StatusUnauthorized},
		{"correct secret", secret, http.StatusOK},
		{"correct with surrounding whitespace", "  " + secret + "  ", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			if tc.header != "" {
				req.Header.Set("X-OpenClaw-Webhook-Token", tc.header)
			}
			h(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d", rec.Code, tc.want)
			}
		})
	}
}

func TestDiscordWebhookNoSecretConfigured(t *testing.T) {
	h := BuildDiscordWebhookHandler("", func(context.Context, InboundMessage) error { return nil })
	rec := httptest.NewRecorder()
	body := `{"content":"hi","channel_id":"c1","author":{"bot":false,"id":"u1"}}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 when no secret configured", rec.Code)
	}
}

func TestTeamsWebhookSecret(t *testing.T) {
	const secret = "teams-s3kr1t"
	h := BuildTeamsWebhookHandler(secret, func(context.Context, InboundMessage) error { return nil })
	body := `{"type":"message","text":"hi","conversation":{"id":"c1"},"from":{"id":"u1"}}`

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"missing header", "", http.StatusUnauthorized},
		{"wrong secret", "nope", http.StatusUnauthorized},
		{"correct secret", secret, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			if tc.header != "" {
				req.Header.Set("X-OpenClaw-Webhook-Token", tc.header)
			}
			h(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d", rec.Code, tc.want)
			}
		})
	}
}

func TestTeamsWebhookNoSecretConfigured(t *testing.T) {
	h := BuildTeamsWebhookHandler("", func(context.Context, InboundMessage) error { return nil })
	rec := httptest.NewRecorder()
	body := `{"type":"message","text":"hi","conversation":{"id":"c1"},"from":{"id":"u1"}}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 when no secret configured", rec.Code)
	}
}

func TestTelegramChannelWebhookSecret(t *testing.T) {
	const secret = "tg-s3kr1t"
	tg := NewTelegramChannel("dummy", "")
	h := tg.BuildWebhookHandler(secret, func(context.Context, InboundMessage) error { return nil }, nil)
	body := `{"update_id":1,"message":{"text":"hi","from":{"is_bot":false},"chat":{"id":42}}}`

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"missing header", "", http.StatusUnauthorized},
		{"wrong secret", "nope", http.StatusUnauthorized},
		{"correct secret", secret, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			if tc.header != "" {
				req.Header.Set("X-Telegram-Bot-Api-Secret-Token", tc.header)
			}
			h(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d", rec.Code, tc.want)
			}
		})
	}
}

func TestTelegramStandaloneWebhookSecret(t *testing.T) {
	const secret = "tg-standalone-s3kr1t"
	h := BuildTelegramWebhookHandler(secret, func(context.Context, InboundMessage) error { return nil }, nil)
	body := `{"update_id":1,"message":{"text":"hi","from":{"is_bot":false},"chat":{"id":42}}}`

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"missing header", "", http.StatusUnauthorized},
		{"wrong secret", "nope", http.StatusUnauthorized},
		{"correct secret", secret, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			if tc.header != "" {
				req.Header.Set("X-Telegram-Bot-Api-Secret-Token", tc.header)
			}
			h(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d", rec.Code, tc.want)
			}
		})
	}
}

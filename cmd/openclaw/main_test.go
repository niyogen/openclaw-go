package main

import (
	"strings"
	"testing"

	"openclaw-go/internal/config"
)

func TestValidateGatewayChannelConfig_WhatsAppVerifyToken(t *testing.T) {
	t.Run("enabled without verify token", func(t *testing.T) {
		cfg := config.Default()
		cfg.Channels.WhatsApp.Enabled = true
		cfg.Channels.WhatsApp.VerifyToken = ""
		err := validateGatewayChannelConfig(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "verify token") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("enabled with verify token", func(t *testing.T) {
		cfg := config.Default()
		cfg.Channels.WhatsApp.Enabled = true
		cfg.Channels.WhatsApp.VerifyToken = "secret"
		if err := validateGatewayChannelConfig(cfg); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("disabled without verify token ok", func(t *testing.T) {
		cfg := config.Default()
		cfg.Channels.WhatsApp.Enabled = false
		cfg.Channels.WhatsApp.VerifyToken = ""
		if err := validateGatewayChannelConfig(cfg); err != nil {
			t.Fatal(err)
		}
	})
}

func TestParseBoolArg(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
		ok   bool
	}{
		{"true", true, true},
		{"TRUE", true, true},
		{"yes", true, true},
		{"1", true, true},
		{"on", true, true},
		{"false", false, true},
		{"no", false, true},
		{"0", false, true},
		{"off", false, true},
		{"maybe", false, false},
		{"", false, false},
	} {
		got, err := parseBoolArg(tc.in)
		if tc.ok {
			if err != nil {
				t.Fatalf("%q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("%q: got %v want %v", tc.in, got, tc.want)
			}
		} else {
			if err == nil {
				t.Fatalf("%q: expected error", tc.in)
			}
		}
	}
}

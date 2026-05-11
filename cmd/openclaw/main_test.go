package main

import (
	"path/filepath"
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

func TestConfigureSetAgentProviderSyncsModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	t.Setenv("OPENCLAW_CONFIG_PATH", path)

	cfg := config.Default()
	cfg.Agent.Provider = "echo"
	cfg.Agent.Model = "echo"
	cfg.Providers.OpenAI.Model = "gpt-4o"
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if err := runConfigure(loaded, []string{"set-agent-provider", "openai"}); err != nil {
		t.Fatal(err)
	}
	after, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if after.Agent.Provider != "openai" {
		t.Fatalf("provider %q", after.Agent.Provider)
	}
	if after.Agent.Model != "gpt-4o" {
		t.Fatalf("model %q want gpt-4o", after.Agent.Model)
	}

	if err := runConfigure(after, []string{"set-agent-provider", "echo"}); err != nil {
		t.Fatal(err)
	}
	afterEcho, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if afterEcho.Agent.Model != "echo" {
		t.Fatalf("echo model %q", afterEcho.Agent.Model)
	}

	cfg2 := config.Default()
	cfg2.Providers.Anthropic.Model = "claude-test-model"
	if err := config.Save(path, cfg2); err != nil {
		t.Fatal(err)
	}
	loaded2, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if err := runConfigure(loaded2, []string{"set-agent-provider", "anthropic"}); err != nil {
		t.Fatal(err)
	}
	afterAnthropic, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if afterAnthropic.Agent.Model != "claude-test-model" {
		t.Fatalf("anthropic model %q", afterAnthropic.Agent.Model)
	}
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

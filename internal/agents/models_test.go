package agents

import (
	"context"
	"testing"
)

func TestKnownModelsNotEmpty(t *testing.T) {
	models := KnownModels()
	if len(models) == 0 {
		t.Fatal("KnownModels should not be empty")
	}
}

func TestModelsForProvider(t *testing.T) {
	openai := ModelsForProvider("openai")
	if len(openai) == 0 {
		t.Fatal("expected openai models")
	}
	for _, m := range openai {
		if m.Provider != "openai" {
			t.Fatalf("unexpected provider %q in openai list", m.Provider)
		}
	}

	anthropic := ModelsForProvider("anthropic")
	if len(anthropic) == 0 {
		t.Fatal("expected anthropic models")
	}

	none := ModelsForProvider("unknown-xyz")
	if len(none) != 0 {
		t.Fatalf("expected no models for unknown provider, got %d", len(none))
	}
}

func TestCapability(t *testing.T) {
	cap := Capability("openai", "sk-test")
	if !cap.Configured {
		t.Fatal("expected configured=true when key provided")
	}
	if len(cap.Features) == 0 {
		t.Fatal("expected features for openai")
	}

	capNoKey := Capability("openai", "")
	if capNoKey.Configured {
		t.Fatal("expected configured=false when key missing")
	}

	echo := Capability("echo", "")
	if !echo.Configured {
		t.Fatal("echo should always be configured")
	}
}

func TestInferWithEchoRunner(t *testing.T) {
	runner := &EchoRunner{}
	reply, err := Infer(context.Background(), runner, "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply == "" {
		t.Fatal("expected non-empty reply")
	}
}

func TestInferEmptyMessage(t *testing.T) {
	runner := &EchoRunner{}
	_, err := Infer(context.Background(), runner, "")
	if err == nil {
		t.Fatal("expected error for empty message")
	}
}

package agents

import (
	"context"
	"fmt"
	"strings"
)

// ModelInfo describes a known model entry for a provider.
type ModelInfo struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Default  bool   `json:"default,omitempty"`
}

// KnownModels returns the built-in catalogue of supported models.
func KnownModels() []ModelInfo {
	return []ModelInfo{
		// Echo
		{Provider: "echo", ID: "echo", Name: "Echo (local, no API)", Default: true},

		// OpenAI
		{Provider: "openai", ID: "gpt-4o", Name: "GPT-4o"},
		{Provider: "openai", ID: "gpt-4o-mini", Name: "GPT-4o Mini", Default: true},
		{Provider: "openai", ID: "gpt-4-turbo", Name: "GPT-4 Turbo"},
		{Provider: "openai", ID: "gpt-3.5-turbo", Name: "GPT-3.5 Turbo"},
		{Provider: "openai", ID: "o1-mini", Name: "o1-mini"},
		{Provider: "openai", ID: "o1", Name: "o1"},

		// Anthropic
		{Provider: "anthropic", ID: "claude-opus-4-5", Name: "Claude Opus 4.5"},
		{Provider: "anthropic", ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5"},
		{Provider: "anthropic", ID: "claude-3-5-haiku-20241022", Name: "Claude 3.5 Haiku", Default: true},
		{Provider: "anthropic", ID: "claude-3-5-sonnet-20241022", Name: "Claude 3.5 Sonnet"},
		{Provider: "anthropic", ID: "claude-3-opus-20240229", Name: "Claude 3 Opus"},
	}
}

// ModelsForProvider returns models filtered to a specific provider.
func ModelsForProvider(provider string) []ModelInfo {
	p := strings.ToLower(strings.TrimSpace(provider))
	var out []ModelInfo
	for _, m := range KnownModels() {
		if strings.ToLower(m.Provider) == p {
			out = append(out, m)
		}
	}
	return out
}

// ProviderCapabilities describes what a provider supports.
type ProviderCapabilities struct {
	Provider   string   `json:"provider"`
	Configured bool     `json:"configured"`
	Features   []string `json:"features"`
	Models     []string `json:"models"`
}

// Capability returns the capability descriptor for a provider given whether its key is set.
func Capability(provider, apiKey string) ProviderCapabilities {
	p := strings.ToLower(strings.TrimSpace(provider))
	configured := strings.TrimSpace(apiKey) != "" || p == "echo"

	switch p {
	case "openai":
		var models []string
		for _, m := range ModelsForProvider("openai") {
			models = append(models, m.ID)
		}
		return ProviderCapabilities{
			Provider:   "openai",
			Configured: configured,
			Features:   []string{"chat", "history", "function-calling", "streaming"},
			Models:     models,
		}
	case "anthropic", "claude":
		var models []string
		for _, m := range ModelsForProvider("anthropic") {
			models = append(models, m.ID)
		}
		return ProviderCapabilities{
			Provider:   "anthropic",
			Configured: configured,
			Features:   []string{"chat", "history", "vision"},
			Models:     models,
		}
	default:
		return ProviderCapabilities{
			Provider:   "echo",
			Configured: true,
			Features:   []string{"chat"},
			Models:     []string{"echo"},
		}
	}
}

// Infer runs a single-turn inference directly against the configured provider.
func Infer(ctx context.Context, runner Runner, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("message cannot be empty")
	}
	return runner.GenerateReply(ctx, Turn{Message: message})
}

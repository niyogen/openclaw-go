package agents

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type HistoryMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Turn struct {
	Message string
	History []HistoryMessage
}

type Runner interface {
	GenerateReply(ctx context.Context, turn Turn) (string, error)
}

type EchoRunner struct{}

func (r *EchoRunner) GenerateReply(_ context.Context, turn Turn) (string, error) {
	trimmed := strings.TrimSpace(turn.Message)
	if trimmed == "" {
		return "I received an empty message.", nil
	}
	return fmt.Sprintf("Echo: %s", trimmed), nil
}

type MultiRunner struct {
	Primary  Runner
	Fallback Runner
}

func (r *MultiRunner) GenerateReply(ctx context.Context, turn Turn) (string, error) {
	if r.Primary != nil {
		reply, err := r.Primary.GenerateReply(ctx, turn)
		if err == nil {
			return reply, nil
		}
	}
	if r.Fallback != nil {
		return r.Fallback.GenerateReply(ctx, turn)
	}
	return "", errors.New("no runner configured")
}

type RunnerOptions struct {
	Provider         string
	OpenAIAPIKey     string
	OpenAIBaseURL    string
	OpenAIModel      string
	AnthropicAPIKey  string
	AnthropicBaseURL string
	AnthropicModel   string
	Client           *http.Client
}

func NewMultiRunner(
	provider string,
	openAIAPIKey,
	openAIBaseURL,
	openAIModel string,
	client *http.Client,
) Runner {
	return NewRunnerFromOptions(RunnerOptions{
		Provider:      provider,
		OpenAIAPIKey:  openAIAPIKey,
		OpenAIBaseURL: openAIBaseURL,
		OpenAIModel:   openAIModel,
		Client:        client,
	})
}

func NewRunnerFromOptions(opts RunnerOptions) Runner {
	echo := &EchoRunner{}
	p := strings.ToLower(strings.TrimSpace(opts.Provider))
	switch p {
	case "openai":
		if strings.TrimSpace(opts.OpenAIAPIKey) == "" {
			return &MultiRunner{Primary: echo}
		}
		return &MultiRunner{
			Primary:  NewOpenAIRunner(OpenAIOptions{APIKey: opts.OpenAIAPIKey, BaseURL: opts.OpenAIBaseURL, Model: opts.OpenAIModel, Client: opts.Client}),
			Fallback: echo,
		}
	case "anthropic", "claude":
		if strings.TrimSpace(opts.AnthropicAPIKey) == "" {
			return &MultiRunner{Primary: echo}
		}
		return &MultiRunner{
			Primary:  NewAnthropicRunner(AnthropicOptions{APIKey: opts.AnthropicAPIKey, BaseURL: opts.AnthropicBaseURL, Model: opts.AnthropicModel, Client: opts.Client}),
			Fallback: echo,
		}
	default:
		return &MultiRunner{Primary: echo}
	}
}

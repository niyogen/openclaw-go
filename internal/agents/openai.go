package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type OpenAIOptions struct {
	APIKey  string
	BaseURL string
	Model   string
	Client  *http.Client
}

type OpenAIRunner struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

func NewOpenAIRunner(opts OpenAIOptions) *OpenAIRunner {
	baseURL := strings.TrimSpace(opts.BaseURL)
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = "gpt-4o-mini"
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &OpenAIRunner{
		apiKey:  strings.TrimSpace(opts.APIKey),
		baseURL: strings.TrimSuffix(baseURL, "/"),
		model:   model,
		client:  client,
	}
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message openAIMessage `json:"message"`
	} `json:"choices"`
}

func (r *OpenAIRunner) GenerateReply(ctx context.Context, turn Turn) (string, error) {
	if r.apiKey == "" {
		return "", fmt.Errorf("openai api key is empty")
	}

	messages := make([]openAIMessage, 0, len(turn.History)+1)
	for _, item := range turn.History {
		if strings.TrimSpace(item.Content) == "" {
			continue
		}
		messages = append(messages, openAIMessage(item))
	}
	messages = append(messages, openAIMessage{
		Role:    "user",
		Content: turn.Message,
	})
	reqBody := openAIChatRequest{
		Model:    r.model,
		Messages: messages,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		r.baseURL+"/chat/completions",
		bytes.NewReader(raw),
	)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("openai returned %d: %s", resp.StatusCode, string(body))
	}
	var decoded openAIChatResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", err
	}
	if len(decoded.Choices) == 0 {
		return "", fmt.Errorf("openai returned no choices")
	}
	return decoded.Choices[0].Message.Content, nil
}

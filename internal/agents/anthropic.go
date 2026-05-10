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

const anthropicBaseURL = "https://api.anthropic.com"
const anthropicDefaultModel = "claude-3-5-haiku-20241022"
const anthropicVersion = "2023-06-01"

type AnthropicOptions struct {
	APIKey  string
	BaseURL string
	Model   string
	Client  *http.Client
}

type AnthropicRunner struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

func NewAnthropicRunner(opts AnthropicOptions) *AnthropicRunner {
	baseURL := strings.TrimSuffix(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		baseURL = anthropicBaseURL
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = anthropicDefaultModel
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &AnthropicRunner{
		apiKey:  strings.TrimSpace(opts.APIKey),
		baseURL: baseURL,
		model:   model,
		client:  client,
	}
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// buildAnthropicMessages converts a Turn into an Anthropic-compatible message
// list.  Anthropic requires strictly alternating user/assistant roles; this
// function enforces that by:
//   - prepending system messages into the next user turn's content
//   - converting tool results into user turns
//   - merging consecutive same-role messages
func buildAnthropicMessages(turn Turn) []anthropicMessage {
	var pendingSystem []string
	messages := make([]anthropicMessage, 0, len(turn.History)+1)
	for _, item := range turn.History {
		role := strings.TrimSpace(item.Role)
		content := strings.TrimSpace(item.Content)
		if content == "" {
			continue
		}
		switch role {
		case "system":
			pendingSystem = append(pendingSystem, content)
		case "tool":
			toolContent := "[tool result] " + content
			if len(pendingSystem) > 0 {
				toolContent = strings.Join(pendingSystem, "\n") + "\n" + toolContent
				pendingSystem = pendingSystem[:0]
			}
			if len(messages) > 0 && messages[len(messages)-1].Role == "user" {
				messages[len(messages)-1].Content += "\n" + toolContent
			} else {
				messages = append(messages, anthropicMessage{Role: "user", Content: toolContent})
			}
		case "user", "assistant":
			userContent := content
			if role == "user" && len(pendingSystem) > 0 {
				userContent = strings.Join(pendingSystem, "\n") + "\n" + content
				pendingSystem = pendingSystem[:0]
			}
			if len(messages) > 0 && messages[len(messages)-1].Role == role {
				messages[len(messages)-1].Content += "\n" + userContent
			} else {
				messages = append(messages, anthropicMessage{Role: role, Content: userContent})
			}
		}
	}
	currentMsg := turn.Message
	if len(pendingSystem) > 0 {
		currentMsg = strings.Join(pendingSystem, "\n") + "\n" + currentMsg
	}
	if len(messages) > 0 && messages[len(messages)-1].Role == "user" {
		messages[len(messages)-1].Content += "\n" + currentMsg
	} else {
		messages = append(messages, anthropicMessage{Role: "user", Content: currentMsg})
	}
	return messages
}

func (r *AnthropicRunner) GenerateReply(ctx context.Context, turn Turn) (string, error) {
	if r.apiKey == "" {
		return "", fmt.Errorf("anthropic api key is empty")
	}

	messages := buildAnthropicMessages(turn)

	reqBody := anthropicRequest{
		Model:     r.model,
		MaxTokens: 4096,
		Messages:  messages,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", r.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)

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
		return "", fmt.Errorf("anthropic returned %d: %s", resp.StatusCode, string(body))
	}
	var decoded anthropicResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", err
	}
	if decoded.Error != nil {
		return "", fmt.Errorf("anthropic error %s: %s", decoded.Error.Type, decoded.Error.Message)
	}
	for _, block := range decoded.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("anthropic returned no text content")
}

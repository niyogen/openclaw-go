package agents

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// StreamReply implements StreamingRunner for OpenAI using the streaming
// chat completions API (text/event-stream).
func (r *OpenAIRunner) StreamReply(ctx context.Context, turn Turn, out chan<- StreamChunk) {
	if r.apiKey == "" {
		out <- StreamChunk{Err: fmt.Errorf("openai api key is empty")}
		return
	}

	messages := buildMessages(turn)
	reqBody := openAIChatRequest{Model: r.model, Messages: messages}
	// Add stream: true to the request body.
	type streamRequest struct {
		openAIChatRequest
		Stream bool `json:"stream"`
	}
	raw, err := json.Marshal(streamRequest{openAIChatRequest: reqBody, Stream: true})
	if err != nil {
		out <- StreamChunk{Err: err}
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		out <- StreamChunk{Err: err}
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := r.client.Do(req)
	if err != nil {
		out <- StreamChunk{Err: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		out <- StreamChunk{Err: fmt.Errorf("openai stream returned %d", resp.StatusCode)}
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if strings.TrimSpace(data) == "[DONE]" {
			out <- StreamChunk{Done: true}
			return
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			out <- StreamChunk{Delta: chunk.Choices[0].Delta.Content}
		}
	}
	if err := scanner.Err(); err != nil {
		out <- StreamChunk{Err: err}
		return
	}
	out <- StreamChunk{Done: true}
}

// buildMessages factors out the history→messages conversion shared by
// GenerateReply and StreamReply.
// turn.Message is only appended when non-empty; tool-loop continuation turns
// have an empty Message and rely solely on history to carry context.
func buildMessages(turn Turn) []openAIMessage {
	msgs := make([]openAIMessage, 0, len(turn.History)+1)
	for _, item := range turn.History {
		if strings.TrimSpace(item.Content) == "" {
			continue
		}
		msgs = append(msgs, openAIMessage(item))
	}
	if strings.TrimSpace(turn.Message) != "" {
		msgs = append(msgs, openAIMessage{Role: "user", Content: turn.Message})
	}
	return msgs
}

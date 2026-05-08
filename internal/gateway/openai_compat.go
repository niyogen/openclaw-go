package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"openclaw-go/internal/agents"
)

// registerOpenAICompatRoutes adds OpenAI-compatible endpoints under /v1/.
// These let existing OpenAI clients point at the gateway without modification.
func (s *Server) registerOpenAICompatRoutes() {
	s.mux.Handle("GET /v1/models", s.withAuth(s.handleV1Models))
	s.mux.Handle("POST /v1/chat/completions", s.withAuth(s.withRateLimit(s.handleV1ChatCompletions)))
	s.mux.HandleFunc("/v1/health", s.handleHealth)
	s.mux.HandleFunc("/v1/healthz", s.handleHealth)
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/ready", s.handleReady)
	s.mux.HandleFunc("/readyz", s.handleReady)
}

// handleReady is a readiness probe — returns 200 as long as the gateway is up.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"ready":   true,
		"service": "openclaw-go-gateway",
		"version": Version,
	})
}

// handleV1Models returns the model catalogue in OpenAI list format.
func (s *Server) handleV1Models(w http.ResponseWriter, _ *http.Request) {
	all := agents.KnownModels()
	data := make([]map[string]any, 0, len(all))
	for _, m := range all {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"owned_by": m.Provider,
			"created":  time.Now().Unix(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

// v1ChatRequest mirrors the OpenAI chat completion request body.
type v1ChatRequest struct {
	Model     string          `json:"model"`
	Messages  []v1ChatMessage `json:"messages"`
	Stream    bool            `json:"stream"`
	MaxTokens *int            `json:"max_tokens,omitempty"`
}

type v1ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// handleV1ChatCompletions proxies through the configured runner and returns
// a response shaped like an OpenAI chat completion object.
func (s *Server) handleV1ChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req v1ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "messages is required"})
		return
	}

	// Build Turn from messages: last message is the prompt, rest is history.
	var history []agents.HistoryMessage
	lastMsg := req.Messages[len(req.Messages)-1]
	for _, m := range req.Messages[:len(req.Messages)-1] {
		history = append(history, agents.HistoryMessage{Role: m.Role, Content: m.Content})
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	reply, err := s.runner.GenerateReply(ctx, agents.Turn{
		Message: lastMsg.Content,
		History: history,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{
				"message": err.Error(),
				"type":    "server_error",
				"code":    "internal_error",
			},
		})
		return
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "openclaw-go"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]string{
					"role":    "assistant",
					"content": reply,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     len(strings.Fields(lastMsg.Content)),
			"completion_tokens": len(strings.Fields(reply)),
			"total_tokens":      len(strings.Fields(lastMsg.Content)) + len(strings.Fields(reply)),
		},
	})
}

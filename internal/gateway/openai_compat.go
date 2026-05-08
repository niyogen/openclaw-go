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

// handleV1ChatCompletions supports both standard (JSON) and streaming (SSE)
// OpenAI chat completion requests.  Set "stream": true in the request body
// to receive Server-Sent Events in the same format as the OpenAI API.
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

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "openclaw-go"
	}

	lastMsg := req.Messages[len(req.Messages)-1]
	var history []agents.HistoryMessage
	for _, m := range req.Messages[:len(req.Messages)-1] {
		history = append(history, agents.HistoryMessage{Role: m.Role, Content: m.Content})
	}
	turn := agents.Turn{Message: lastMsg.Content, History: history}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	if req.Stream {
		s.handleV1ChatStream(w, ctx, model, lastMsg.Content, turn)
		return
	}
	s.handleV1ChatBlocking(w, ctx, model, lastMsg.Content, turn)
}

func (s *Server) handleV1ChatBlocking(
	w http.ResponseWriter,
	ctx context.Context,
	model, prompt string,
	turn agents.Turn,
) {
	reply, err := s.runner.GenerateReply(ctx, turn)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": reply},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens":     len(strings.Fields(prompt)),
			"completion_tokens": len(strings.Fields(reply)),
			"total_tokens":      len(strings.Fields(prompt)) + len(strings.Fields(reply)),
		},
	})
}

func (s *Server) handleV1ChatStream(
	w http.ResponseWriter,
	ctx context.Context,
	model, prompt string,
	turn agents.Turn,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)

	writeDelta := func(delta string, finishReason *string) {
		choice := map[string]any{
			"index": 0,
			"delta": map[string]string{"role": "assistant", "content": delta},
		}
		if finishReason != nil {
			choice["finish_reason"] = *finishReason
			choice["delta"] = map[string]string{}
		}
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{choice},
		}
		raw, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		flusher.Flush()
	}

	// Send the role header chunk first.
	writeDelta("", nil)

	out := make(chan agents.StreamChunk, 32)
	go func() {
		defer close(out)
		agents.Stream(ctx, s.runner, turn, out)
	}()

	for chunk := range out {
		if chunk.Err != nil {
			errMsg := map[string]any{
				"error": map[string]any{"message": chunk.Err.Error(), "type": "server_error"},
			}
			raw, _ := json.Marshal(errMsg)
			fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
			return
		}
		if chunk.Done {
			stop := "stop"
			writeDelta("", &stop)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		writeDelta(chunk.Delta, nil)
	}
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"openclaw-go/internal/agents"
	"openclaw-go/internal/runtime"
)

// registerOpenAICompatRoutes adds OpenAI-compatible endpoints under /v1/.
func (s *Server) registerOpenAICompatRoutes() {
	cors := s.withCORSMiddleware
	s.mux.Handle("GET /v1/models", cors(s.withAuth(s.handleV1Models)))
	s.mux.Handle("POST /v1/chat/completions", cors(s.withAuth(s.withRateLimit(withBodyLimit(s.handleV1ChatCompletions)))))
	s.mux.Handle("POST /v1/responses", cors(s.withAuth(s.withRateLimit(withBodyLimit(s.handleV1Responses)))))
	s.mux.Handle("POST /v1/embeddings", cors(s.withAuth(withBodyLimit(s.handleV1Embeddings))))
	s.mux.HandleFunc("/v1/health", s.handleHealth)
	s.mux.HandleFunc("/v1/healthz", s.handleHealth)
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/ready", s.handleReady)
	s.mux.HandleFunc("/readyz", s.handleReady)
}

// extractToolCallsFromDelta tries to parse tool_calls from a raw model response delta.
func extractToolCallsFromDelta(raw string) []runtime.ToolCallRequest {
	return runtime.ExtractToolCalls(raw)
}

// writeToolCallDelta emits tool_call deltas in OpenAI streaming format.
func writeToolCallDelta(w http.ResponseWriter, flusher http.Flusher, id string, created int64, model string, calls []runtime.ToolCallRequest) {
	toolCalls := make([]map[string]any, 0, len(calls))
	for i, tc := range calls {
		toolCalls = append(toolCalls, map[string]any{
			"index": i,
			"id":    tc.ID,
			"type":  "function",
			"function": map[string]string{
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			},
		})
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "tool_calls": toolCalls},
			"finish_reason": "tool_calls",
		}},
	}
	raw, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", raw)
	flusher.Flush()
}

// handleV1Responses implements the OpenAI Responses API (create a model response).
// This is a simplified version that maps to our chat completion path.
// See https://platform.openai.com/docs/api-reference/responses
func (s *Server) handleV1Responses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req struct {
		Model  string `json:"model"`
		Input  string `json:"input"` // simple text input
		Stream bool   `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "input is required"})
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "openclaw-go"
	}
	turn := agents.Turn{Message: req.Input}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	if req.Stream {
		s.handleV1ChatStream(w, ctx, model, req.Input, turn)
		return
	}
	reply, err := s.globalRunner().GenerateReply(ctx, turn)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "server_error"},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     fmt.Sprintf("resp-%d", time.Now().UnixNano()),
		"object": "response",
		"model":  model,
		"output": []map[string]any{{
			"type":    "message",
			"role":    "assistant",
			"content": []map[string]string{{"type": "output_text", "text": reply}},
		}},
		"usage": map[string]int{
			"input_tokens":  len(strings.Fields(req.Input)),
			"output_tokens": len(strings.Fields(reply)),
			"total_tokens":  len(strings.Fields(req.Input)) + len(strings.Fields(reply)),
		},
	})
}

// handleV1Embeddings proxies embedding requests through the configured runner.
// When the runner supports embeddings natively it is used; otherwise a simple
// averaged bag-of-words embedding is returned for compatibility.
func (s *Server) handleV1Embeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var raw struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if len(raw.Input) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "input is required"})
		return
	}
	// Accept either a JSON array of strings or a single string.
	var inputs []string
	if err := json.Unmarshal(raw.Input, &inputs); err != nil {
		var single string
		if err2 := json.Unmarshal(raw.Input, &single); err2 != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "input must be a string or array of strings"})
			return
		}
		inputs = []string{single}
	}
	req := struct {
		Model string
		Input []string
	}{Model: raw.Model, Input: inputs}
	if len(req.Input) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "input is required"})
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "text-embedding-ada-002"
	}

	// Generate a deterministic placeholder embedding (256 dims) for each input.
	// Replace with real provider call when an embedding runner is added.
	data := make([]map[string]any, 0, len(req.Input))
	for i, text := range req.Input {
		embedding := pseudoEmbedding(text, 256)
		data = append(data, map[string]any{
			"object":    "embedding",
			"index":     i,
			"embedding": embedding,
		})
	}
	totalTokens := 0
	for _, t := range req.Input {
		totalTokens += len(strings.Fields(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
		"model":  model,
		"usage":  map[string]int{"prompt_tokens": totalTokens, "total_tokens": totalTokens},
	})
}

// pseudoEmbedding returns a deterministic float32 slice of length dims
// derived from the character byte values of text. Not a real embedding —
// replace with an actual model call for production use.
func pseudoEmbedding(text string, dims int) []float64 {
	vec := make([]float64, dims)
	if len(text) == 0 {
		return vec
	}
	for i, b := range []byte(text) {
		vec[i%dims] += float64(b) / 255.0
	}
	// L2-normalise: divide each element by ‖v‖ = sqrt(sum of squares).
	norm := 0.0
	for _, v := range vec {
		norm += v * v
	}
	if norm > 0 {
		scale := 1.0 / math.Sqrt(norm)
		for i := range vec {
			vec[i] *= scale
		}
	}
	return vec
}

// handleReady is a readiness probe — checks that the session store is
// accessible and the runner is configured before returning 200.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	checks := map[string]any{}
	allOK := true

	// Check session store is accessible.
	sessions := s.store.List()
	checks["sessions"] = map[string]any{"ok": true, "count": len(sessions)}

	// Check runner is configured.
	if s.globalRunner() == nil {
		checks["runner"] = map[string]any{"ok": false, "reason": "no runner configured"}
		allOK = false
	} else {
		checks["runner"] = map[string]any{"ok": true}
	}

	// Check log store is accessible.
	if s.logs == nil {
		checks["logs"] = map[string]any{"ok": false}
		allOK = false
	} else {
		checks["logs"] = map[string]any{"ok": true}
	}

	status := http.StatusOK
	if !allOK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"ok":      allOK,
		"status":  "ready",
		"service": "openclaw-go-gateway",
		"version": Version,
		"checks":  checks,
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

	// Build Turn respecting the role of every message.
	// All messages go into History; if the last message is from the user it
	// is also set as turn.Message (the current prompt).  Other roles (assistant,
	// system) are left in history only so buildMessages does not double-append.
	var history []agents.HistoryMessage
	for _, m := range req.Messages {
		history = append(history, agents.HistoryMessage{Role: m.Role, Content: m.Content})
	}
	lastMsg := req.Messages[len(req.Messages)-1]
	currentMsg := ""
	if strings.ToLower(strings.TrimSpace(lastMsg.Role)) == "user" {
		currentMsg = lastMsg.Content
		// Remove the last user message from history to avoid duplication with
		// turn.Message (buildMessages appends turn.Message separately).
		history = history[:len(history)-1]
	}
	turn := agents.Turn{Message: currentMsg, History: history}
	if req.MaxTokens != nil {
		turn.MaxTokens = *req.MaxTokens
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	if req.Stream {
		s.handleV1ChatStream(w, ctx, model, currentMsg, turn)
		return
	}
	s.handleV1ChatBlocking(w, ctx, model, currentMsg, turn)
}

// countTokensApprox counts whitespace-delimited words across the full turn
// (history + current message) as a cheap approximation of prompt tokens.
func countTokensApprox(turn agents.Turn) int {
	n := len(strings.Fields(turn.Message))
	for _, h := range turn.History {
		n += len(strings.Fields(h.Content))
	}
	return n
}

func (s *Server) handleV1ChatBlocking(
	w http.ResponseWriter,
	ctx context.Context,
	model, prompt string,
	turn agents.Turn,
) {
	reply, err := s.globalRunner().GenerateReply(ctx, turn)
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
			"prompt_tokens":     countTokensApprox(turn),
			"completion_tokens": len(strings.Fields(reply)),
			"total_tokens":      countTokensApprox(turn) + len(strings.Fields(reply)),
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
		agents.Stream(ctx, s.globalRunner(), turn, out)
	}()

	// replyWords accumulates the completion token count using the same
	// word-split approximation as the blocking path for consistency.
	var replyWords int
	for chunk := range out {
		if chunk.Err != nil {
			errMsg := map[string]any{
				"error": map[string]any{"message": chunk.Err.Error(), "type": "server_error"},
			}
			raw, _ := json.Marshal(errMsg)
			fmt.Fprintf(w, "data: %s\n\n", raw)
			fmt.Fprintf(w, "data: [DONE]\n\n") // always terminate SSE stream
			flusher.Flush()
			return
		}
		if chunk.Done {
			stop := "stop"
			writeDelta("", &stop)
			// Emit usage before [DONE] so clients can track token consumption.
			promptToks := countTokensApprox(turn)
			usageChunk := map[string]any{
				"id":      id,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []any{},
				"usage": map[string]int{
					"prompt_tokens":     promptToks,
					"completion_tokens": replyWords,
					"total_tokens":      promptToks + replyWords,
				},
			}
			raw, _ := json.Marshal(usageChunk)
			fmt.Fprintf(w, "data: %s\n\n", raw)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		// Check if this delta is a tool_call JSON chunk.
		if strings.HasPrefix(strings.TrimSpace(chunk.Delta), "{") {
			toolCalls := extractToolCallsFromDelta(chunk.Delta)
			if len(toolCalls) > 0 {
				writeToolCallDelta(w, flusher, id, created, model, toolCalls)
				continue
			}
		}
		replyWords += len(strings.Fields(chunk.Delta))
		writeDelta(chunk.Delta, nil)
	}
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

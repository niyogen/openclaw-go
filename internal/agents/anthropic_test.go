package agents

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnthropicRunnerReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		resp := map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "Hello from Claude"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	runner := NewAnthropicRunner(AnthropicOptions{
		APIKey:  "test-key",
		BaseURL: srv.URL,
		Model:   "claude-test",
		Client:  srv.Client(),
	})
	reply, err := runner.GenerateReply(context.Background(), Turn{Message: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply != "Hello from Claude" {
		t.Fatalf("unexpected reply: %q", reply)
	}
}

func TestAnthropicRunnerMissingKey(t *testing.T) {
	runner := NewAnthropicRunner(AnthropicOptions{})
	_, err := runner.GenerateReply(context.Background(), Turn{Message: "hi"})
	if err == nil {
		t.Fatal("expected error for missing api key")
	}
}

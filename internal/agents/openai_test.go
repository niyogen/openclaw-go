package agents

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIRunner_GenerateReply_MockServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req openAIChatRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Model == "" || len(req.Messages) == 0 {
			t.Errorf("missing model or messages: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIChatResponse{
			Choices: []struct {
				Message openAIMessage `json:"message"`
			}{
				{Message: openAIMessage{Role: "assistant", Content: "mock-reply"}},
			},
		})
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	runner := NewOpenAIRunner(OpenAIOptions{
		APIKey:  "test-key",
		BaseURL: strings.TrimSuffix(srv.URL, "/") + "/v1",
		Model:   "gpt-test-model",
		Client:  client,
	})

	reply, err := runner.GenerateReply(context.Background(), Turn{
		Message: "hello",
		History: []HistoryMessage{{Role: "user", Content: "prior"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "mock-reply" {
		t.Fatalf("reply %q", reply)
	}
}

func TestOpenAIRunner_GenerateReply_EmptyAPIKey(t *testing.T) {
	runner := NewOpenAIRunner(OpenAIOptions{APIKey: "", Model: "x"})
	_, err := runner.GenerateReply(context.Background(), Turn{Message: "hi"})
	if err == nil || !strings.Contains(err.Error(), "api key") {
		t.Fatalf("expected api key error, got %v", err)
	}
}

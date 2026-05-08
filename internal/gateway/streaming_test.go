package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestV1ChatCompletionsBlocking(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"model":"echo","messages":[{"role":"user","content":"hello blocking"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Object != "chat.completion" {
		t.Fatalf("expected chat.completion, got %q", result.Object)
	}
	if len(result.Choices) == 0 || result.Choices[0].Message.Content == "" {
		t.Fatal("empty reply")
	}
}

func TestV1ChatCompletionsStreaming(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"model":"echo","stream":true,"messages":[{"role":"user","content":"hello stream"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	var chunks []string
	doneReceived := false
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			doneReceived = true
			break
		}
		chunks = append(chunks, data)
	}

	if !doneReceived {
		t.Fatal("stream did not end with [DONE]")
	}
	if len(chunks) == 0 {
		t.Fatal("no SSE chunks received")
	}

	// Validate each chunk is valid JSON with the right shape.
	for _, raw := range chunks {
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct{ Content string } `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
			t.Fatalf("invalid chunk JSON: %q: %v", raw, err)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("expected chat.completion.chunk, got %q", chunk.Object)
		}
	}
}

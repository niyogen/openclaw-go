package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"openclaw-go/internal/agents"
	"openclaw-go/internal/channels"
	"openclaw-go/internal/plugins"
	"openclaw-go/internal/sessions"
)

func TestV1Models(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models: %d", resp.StatusCode)
	}
	var result struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Object != "list" {
		t.Fatalf("expected object=list, got %q", result.Object)
	}
	if len(result.Data) == 0 {
		t.Fatal("expected models in data")
	}
}

func TestV1ChatCompletions(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"model":"echo","messages":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v1/chat/completions: %d  %s", resp.StatusCode, raw)
	}
	var result struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Object != "chat.completion" {
		t.Fatalf("expected object=chat.completion, got %q", result.Object)
	}
	if len(result.Choices) == 0 || result.Choices[0].Message.Content == "" {
		t.Fatal("expected non-empty reply in choices")
	}
}

func TestV1Responses(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"model":"echo","input":"hello responses api"}`
	resp, err := http.Post(ts.URL+"/v1/responses", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v1/responses: %d %s", resp.StatusCode, raw)
	}
	var result struct {
		Object string `json:"object"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage map[string]int `json:"usage"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Object != "response" {
		t.Fatalf("expected object=response, got %q", result.Object)
	}
	if len(result.Output) == 0 || result.Output[0].Role != "assistant" {
		t.Fatalf("expected one assistant output; got %+v", result.Output)
	}
	if len(result.Output[0].Content) == 0 || result.Output[0].Content[0].Text == "" {
		t.Fatalf("expected non-empty output_text")
	}
	if result.Usage["total_tokens"] == 0 {
		t.Fatalf("expected non-zero total_tokens in usage")
	}
}

func TestV1ResponsesRejectsMissingInput(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"model":"echo"}` // no input
	resp, err := http.Post(ts.URL+"/v1/responses", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing input, got %d", resp.StatusCode)
	}
}

func TestV1EmbeddingsSingleInput(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"model":"text-embedding-3-small","input":"the quick brown fox"}`
	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v1/embeddings: %d %s", resp.StatusCode, raw)
	}
	var result struct {
		Object string `json:"object"`
		Data   []struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Model string `json:"model"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Object != "list" {
		t.Fatalf("expected object=list, got %q", result.Object)
	}
	if len(result.Data) != 1 {
		t.Fatalf("expected 1 embedding, got %d", len(result.Data))
	}
	if len(result.Data[0].Embedding) != 256 {
		t.Fatalf("expected 256-dim embedding, got %d", len(result.Data[0].Embedding))
	}
}

func TestV1EmbeddingsBatch(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"model":"text-embedding-3-small","input":["alpha","beta","gamma"]}`
	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("batch POST: %d %s", resp.StatusCode, raw)
	}
	var result struct {
		Data []struct {
			Index int `json:"index"`
		} `json:"data"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Data) != 3 {
		t.Fatalf("expected 3 embeddings, got %d", len(result.Data))
	}
	// Indices must be 0,1,2 in order.
	for i, item := range result.Data {
		if item.Index != i {
			t.Fatalf("data[%d].index = %d, want %d", i, item.Index, i)
		}
	}
}

func TestV1EmbeddingsRejectsEmptyInput(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"model":"text-embedding-3-small"}` // no input field
	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing input, got %d", resp.StatusCode)
	}
}

// TestV1EmbeddingsUsesRealProviderWhenAvailable proves the gateway prefers
// the real OpenAI embeddings provider over the pseudo placeholder when the
// active runner implements agents.Embedder and returns vectors.
func TestV1EmbeddingsUsesRealProviderWhenAvailable(t *testing.T) {
	// Fake OpenAI embeddings endpoint — returns a vector that's clearly
	// distinguishable from pseudoEmbedding's 256-dim output.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[9.99,9.99,9.99]}]}`))
	}))
	t.Cleanup(upstream.Close)

	// Build a server whose globalRunner is an OpenAIRunner pointed at the
	// fake upstream. buildTestServer uses EchoRunner, so we rebuild a small
	// server manually here.
	dir := testDataDir(t)
	store, err := sessions.New(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry := plugins.NewRegistry()
	registry.Register(plugins.NewMetaPlugin(registry))
	runner := agents.NewOpenAIRunner(agents.OpenAIOptions{
		APIKey:  "sk-test",
		BaseURL: upstream.URL,
	})
	s := New(
		"127.0.0.1",
		0,
		"",
		[]string{"http://127.0.0.1"},
		store,
		runner,
		channels.NewRouter(),
		registry,
		dir,
	)

	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"model":"text-embedding-3-small","input":"hello"}`
	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Data) != 1 {
		t.Fatalf("expected 1 embedding, got %d", len(result.Data))
	}
	emb := result.Data[0].Embedding
	if len(emb) != 3 {
		t.Fatalf("expected 3-dim provider embedding (pseudo would be 256), got %d dims", len(emb))
	}
	if emb[0] != 9.99 {
		t.Fatalf("expected provider's distinct value 9.99, got %v", emb)
	}
}

// TestV1EmbeddingsFallsBackToPseudoWhenRunnerLacksEmbedder confirms a
// runner without an Embed method doesn't break the endpoint — instead the
// pseudo 256-dim deterministic vector is used. EchoRunner has no Embed.
func TestV1EmbeddingsFallsBackToPseudoWhenRunnerLacksEmbedder(t *testing.T) {
	s := buildTestServer(t, "") // EchoRunner — does not satisfy Embedder
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json",
		bytes.NewBufferString(`{"model":"text-embedding-3-small","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &result)
	if len(result.Data) != 1 || len(result.Data[0].Embedding) != 256 {
		t.Fatalf("expected pseudo 256-dim fallback; got %d dims", len(result.Data[0].Embedding))
	}
}

func TestReadinessProbe(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	for _, path := range []string{"/ready", "/readyz", "/healthz", "/v1/healthz"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d", path, resp.StatusCode)
		}
	}
}

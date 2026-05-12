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

func TestOpenAIEmbedHappyPath(t *testing.T) {
	var seenPath, seenAuth, seenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		_, _ = w.Write([]byte(`{
			"object":"list",
			"data":[
				{"index":0,"object":"embedding","embedding":[0.1,0.2,0.3]},
				{"index":1,"object":"embedding","embedding":[0.4,0.5,0.6]}
			]
		}`))
	}))
	t.Cleanup(srv.Close)

	r := NewOpenAIRunner(OpenAIOptions{
		APIKey:  "sk-test",
		BaseURL: srv.URL,
	})
	vectors, err := r.Embed(context.Background(), "text-embedding-3-small", []string{"alpha", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/embeddings" {
		t.Fatalf("path: %s", seenPath)
	}
	if seenAuth != "Bearer sk-test" {
		t.Fatalf("auth header: %s", seenAuth)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(seenBody), &got)
	if got["model"] != "text-embedding-3-small" {
		t.Fatalf("model: %v", got["model"])
	}
	if len(vectors) != 2 {
		t.Fatalf("vectors len: got %d want 2", len(vectors))
	}
	if len(vectors[0]) != 3 || vectors[0][0] != 0.1 {
		t.Fatalf("vector[0]: %v", vectors[0])
	}
	if len(vectors[1]) != 3 || vectors[1][2] != 0.6 {
		t.Fatalf("vector[1]: %v", vectors[1])
	}
}

func TestOpenAIEmbedNoKeyReturnsNilForFallback(t *testing.T) {
	r := NewOpenAIRunner(OpenAIOptions{}) // no api key
	vectors, err := r.Embed(context.Background(), "text-embedding-3-small", []string{"x"})
	if err != nil {
		t.Fatalf("missing key should not error (caller falls back): %v", err)
	}
	if vectors != nil {
		t.Fatalf("expected nil vectors when no key configured; got %v", vectors)
	}
}

func TestOpenAIEmbedEmptyInputReturnsNil(t *testing.T) {
	r := NewOpenAIRunner(OpenAIOptions{APIKey: "sk-test"})
	vectors, err := r.Embed(context.Background(), "model", nil)
	if err != nil || vectors != nil {
		t.Fatalf("empty input: vectors=%v err=%v", vectors, err)
	}
}

func TestOpenAIEmbedDefaultModel(t *testing.T) {
	var seenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1.0]}]}`))
	}))
	t.Cleanup(srv.Close)

	r := NewOpenAIRunner(OpenAIOptions{APIKey: "sk-test", BaseURL: srv.URL})
	_, _ = r.Embed(context.Background(), "", []string{"x"})
	if !strings.Contains(seenBody, `"model":"text-embedding-3-small"`) {
		t.Fatalf("default model not used; body=%s", seenBody)
	}
}

func TestOpenAIEmbedServerErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	r := NewOpenAIRunner(OpenAIOptions{APIKey: "sk-test", BaseURL: srv.URL})
	_, err := r.Embed(context.Background(), "model", []string{"x"})
	if err == nil {
		t.Fatal("expected error from 429")
	}
}

func TestOpenAIEmbedRespectsIndexOrdering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reply with embeddings out of order — Embed must reorder by index.
		_, _ = w.Write([]byte(`{"data":[
			{"index":2,"embedding":[2.0]},
			{"index":0,"embedding":[0.0]},
			{"index":1,"embedding":[1.0]}
		]}`))
	}))
	t.Cleanup(srv.Close)

	r := NewOpenAIRunner(OpenAIOptions{APIKey: "sk-test", BaseURL: srv.URL})
	vectors, err := r.Embed(context.Background(), "model", []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 3 {
		t.Fatalf("expected 3 vectors, got %d", len(vectors))
	}
	for i, want := range []float64{0.0, 1.0, 2.0} {
		if len(vectors[i]) != 1 || vectors[i][0] != want {
			t.Fatalf("vector[%d] = %v, want [%v]", i, vectors[i], want)
		}
	}
}

func TestOpenAIRunnerImplementsEmbedder(t *testing.T) {
	// Compile-time assertion as a runtime check so a refactor that breaks
	// the interface satisfaction is caught here, not at the gateway.
	var _ Embedder = (*OpenAIRunner)(nil)
}

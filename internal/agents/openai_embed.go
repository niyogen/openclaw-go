package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Embedder is implemented by runners that can produce real vector embeddings
// for a slice of strings. The gateway's /v1/embeddings handler type-asserts
// the active runner to this interface and uses the real implementation when
// available, falling back to a deterministic pseudo-embedding otherwise.
//
// Returning an empty result + nil error is treated as "no embedding produced"
// by the caller and triggers the pseudo fallback — so providers that wish to
// silently decline can do so without invoking the gateway's error path.
type Embedder interface {
	Embed(ctx context.Context, model string, inputs []string) ([][]float64, error)
}

// openAIEmbeddingRequest mirrors OpenAI's `POST /v1/embeddings` body shape.
type openAIEmbeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// openAIEmbeddingResponse captures only the fields we read back — extra
// fields (object/usage) are ignored so a server returning additional keys
// doesn't break the decode.
type openAIEmbeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}

// Embed posts `inputs` to the OpenAI embeddings endpoint (or whatever
// `BaseURL` was configured at construction — typically a drop-in compatible
// proxy like Azure OpenAI or a local LiteLLM) and returns the resulting
// vectors in input order.
//
// If `model` is empty, "text-embedding-3-small" is used. The runner's
// `apiKey` is required; without it Embed returns an empty result + nil
// error so the gateway transparently falls back to its pseudo embedding.
func (r *OpenAIRunner) Embed(ctx context.Context, model string, inputs []string) ([][]float64, error) {
	if r.apiKey == "" {
		// Signal "no real embedder available" without triggering the
		// caller's error path. The gateway treats this as fall-back-permission.
		return nil, nil
	}
	if len(inputs) == 0 {
		return nil, nil
	}
	if model == "" {
		model = "text-embedding-3-small"
	}
	body := openAIEmbeddingRequest{Model: model, Input: inputs}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/embeddings", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.apiKey)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai embed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("openai embed: %d: %s", resp.StatusCode, string(respBody))
	}
	var decoded openAIEmbeddingResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, fmt.Errorf("openai embed: parse response: %w", err)
	}
	out := make([][]float64, len(inputs))
	for _, d := range decoded.Data {
		if d.Index < 0 || d.Index >= len(inputs) {
			continue
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}

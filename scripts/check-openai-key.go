// Command: go run ./scripts/check-openai-key.go
// Verifies an OpenAI API key with GET /v1/models (same auth as chat completions).
// Resolution order: OPENAI_API_KEY / OPENAI_BASE_URL env, then openclaw.json (OPENCLAW_CONFIG_PATH or default path).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"openclaw-go/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "check-openai-key: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OpenAI key OK (GET /v1/models returned 200).")
	os.Exit(0)
}

func run() error {
	cfg, err := config.Load("")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(cfg.Providers.OpenAI.APIKey)
	}
	if apiKey == "" {
		return fmt.Errorf("no API key: set OPENAI_API_KEY or providers.openai.apiKey in openclaw.json")
	}

	base := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if base == "" {
		base = strings.TrimSpace(cfg.Providers.OpenAI.BaseURL)
	}
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	base = strings.TrimSuffix(base, "/")
	url := base + "/models"

	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		var env struct {
			Data []json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(body, &env)
		n := len(env.Data)
		if n > 0 {
			fmt.Fprintf(os.Stderr, "Listed %d models (showing key is accepted).\n", n)
		}
		return nil
	}

	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 400 {
		snippet = snippet[:400] + "…"
	}
	return fmt.Errorf("HTTP %s from %s — %s", resp.Status, url, snippet)
}

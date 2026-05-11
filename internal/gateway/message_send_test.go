package gateway

import (
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

// Quick infer in /ui/ calls JSON-RPC message.send — same path as POST /message.
//
// These tests cover distinct situations (the earlier confusion was mixing them):
//  1) Echo-only gateway → reply is always "Echo: <msg>" (local/dev default).
//  2) OpenAI runner + successful HTTP completion → reply is model text, never "Echo:"-prefixed.
//  3) OpenAI runner + HTTP/API failure → MultiRunner falls back to EchoRunner → looks like (1)
//     even though agent.provider is "openai". This matches a broken key / network / quota.
//  4) Channel "ui" ignores per-session provider overrides so Quick infer matches
//     gateway agent config (regression for sticky echo on session ui-infer).

func TestRPCMessageSend_EchoRunner_MatchesQuickInferEchoSemantics(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"jsonrpc":"2.0","id":1,"method":"message.send","params":{"sessionId":"ui-parity","message":"hi","channel":"ui"}}`
	resp, err := http.Post(ts.URL+"/rpc", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	var envelope struct {
		Result map[string]any `json:"result"`
		Error  *rpcError      `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error != nil {
		t.Fatalf("rpc error: %+v", envelope.Error)
	}
	reply, _ := envelope.Result["reply"].(string)
	want := "Echo: hi"
	if reply != want {
		t.Fatalf("reply %q want %q (echo provider / Quick infer baseline)", reply, want)
	}
}

func TestRPCMessageSend_OpenAIRunner_MockAPI_NoEchoPrefix(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "Hello from mock OpenAI."}},
			},
		})
	}))
	t.Cleanup(mock.Close)

	runner := agents.NewRunnerFromOptions(agents.RunnerOptions{
		Provider:      "openai",
		OpenAIAPIKey:  "sk-test-not-real",
		OpenAIBaseURL: mock.URL + "/v1",
		OpenAIModel:   "gpt-4o-mini",
	})
	s := testServerWithRunner(t, "", runner)
	s.SetAgentSummary("openai", "gpt-4o-mini", true, false)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"jsonrpc":"2.0","id":1,"method":"message.send","params":{"sessionId":"openai-ui","message":"hi","channel":"ui"}}`
	resp, err := http.Post(ts.URL+"/rpc", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	var envelope struct {
		Result map[string]any `json:"result"`
		Error  *rpcError      `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error != nil {
		t.Fatalf("rpc error: %+v", envelope.Error)
	}
	reply, _ := envelope.Result["reply"].(string)
	if reply == "" {
		t.Fatal("expected non-empty reply")
	}
	if strings.HasPrefix(reply, "Echo:") {
		t.Fatalf("expected real model reply, got echo fallback: %q", reply)
	}
	if reply != "Hello from mock OpenAI." {
		t.Fatalf("reply %q", reply)
	}
}

// Regression: /ui Quick infer uses sessionId "ui-infer" forever. If sessions.json
// still has provider/model echo from an older run, runnerForSession would keep
// echoing even after openclaw.json switches agent.provider to openai. Channel
// "ui" must always use the gateway-wide runner (runnerForProcessMessage).
func TestRPCMessageSend_UIChannel_IgnoresStaleEchoSessionOverrideUsesGlobalOpenAI(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "from-global-openai"}},
			},
		})
	}))
	t.Cleanup(mock.Close)

	apiKey := "sk-test-not-real"
	base := mock.URL + "/v1"
	globalRunner := agents.NewRunnerFromOptions(agents.RunnerOptions{
		Provider:      "openai",
		OpenAIAPIKey:  apiKey,
		OpenAIBaseURL: base,
		OpenAIModel:   "gpt-4o-mini",
	})
	s := testServerWithRunner(t, "", globalRunner)
	s.SetAgentSummary("openai", "gpt-4o-mini", true, false)
	s.SetRunnerFactory(func(provider, model string) agents.Runner {
		return agents.NewRunnerFromOptions(agents.RunnerOptions{
			Provider:      provider,
			OpenAIAPIKey:  apiKey,
			OpenAIBaseURL: base,
			OpenAIModel:   model,
		})
	})
	if err := s.store.UpsertSession("ui-infer", "ui", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetSessionModel("ui-infer", "echo", "echo"); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"jsonrpc":"2.0","id":1,"method":"message.send","params":{"sessionId":"ui-infer","message":"hi","channel":"ui"}}`
	resp, err := http.Post(ts.URL+"/rpc", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	var envelope struct {
		Result map[string]any `json:"result"`
		Error  *rpcError      `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error != nil {
		t.Fatalf("rpc error: %+v", envelope.Error)
	}
	reply, _ := envelope.Result["reply"].(string)
	if strings.HasPrefix(reply, "Echo:") {
		t.Fatalf("ui channel must not use stale echo session override, got %q", reply)
	}
	if reply != "from-global-openai" {
		t.Fatalf("reply %q want from-global-openai", reply)
	}
}

// Non-ui channels must still honor per-session model overrides (e.g. experiments).
func TestRPCMessageSend_CLIChannel_RespectsEchoSessionOverride(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "should-not-reach"}},
			},
		})
	}))
	t.Cleanup(mock.Close)

	apiKey := "sk-test-not-real"
	base := mock.URL + "/v1"
	globalRunner := agents.NewRunnerFromOptions(agents.RunnerOptions{
		Provider:      "openai",
		OpenAIAPIKey:  apiKey,
		OpenAIBaseURL: base,
		OpenAIModel:   "gpt-4o-mini",
	})
	s := testServerWithRunner(t, "", globalRunner)
	s.SetAgentSummary("openai", "gpt-4o-mini", true, false)
	s.SetRunnerFactory(func(provider, model string) agents.Runner {
		return agents.NewRunnerFromOptions(agents.RunnerOptions{
			Provider:      provider,
			OpenAIAPIKey:  apiKey,
			OpenAIBaseURL: base,
			OpenAIModel:   model,
		})
	})
	if err := s.store.UpsertSession("sticky-cli", "cli", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetSessionModel("sticky-cli", "echo", "echo"); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"jsonrpc":"2.0","id":1,"method":"message.send","params":{"sessionId":"sticky-cli","message":"hi","channel":"cli"}}`
	resp, err := http.Post(ts.URL+"/rpc", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	var envelope struct {
		Result map[string]any `json:"result"`
		Error  *rpcError      `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error != nil {
		t.Fatalf("rpc error: %+v", envelope.Error)
	}
	reply, _ := envelope.Result["reply"].(string)
	if reply != "Echo: hi" {
		t.Fatalf("cli channel should keep echo session override, got %q", reply)
	}
}

func TestRPCMessageSend_OpenAIRunner_MockAPI_HTTPError_FallsBackToEcho(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, `{"error":{"message":"invalid_api_key"}}`, http.StatusUnauthorized)
	}))
	t.Cleanup(mock.Close)

	runner := agents.NewRunnerFromOptions(agents.RunnerOptions{
		Provider:      "openai",
		OpenAIAPIKey:  "sk-invalid",
		OpenAIBaseURL: mock.URL + "/v1",
		OpenAIModel:   "gpt-4o-mini",
	})
	s := testServerWithRunner(t, "", runner)
	s.SetAgentSummary("openai", "gpt-4o-mini", true, false)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"jsonrpc":"2.0","id":1,"method":"message.send","params":{"sessionId":"fallback-ui","message":"hi","channel":"ui"}}`
	resp, err := http.Post(ts.URL+"/rpc", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	var envelope struct {
		Result map[string]any `json:"result"`
		Error  *rpcError      `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error != nil {
		t.Fatalf("rpc error: %+v", envelope.Error)
	}
	reply, _ := envelope.Result["reply"].(string)
	if reply != "Echo: hi" {
		t.Fatalf("expected echo fallback after OpenAI failure, got %q", reply)
	}
}

func testServerWithRunner(t *testing.T, authToken string, runner agents.Runner) *Server {
	t.Helper()
	dir := testDataDir(t)
	store, err := sessions.New(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("sessions.New failed: %v", err)
	}
	registry := plugins.NewRegistry()
	registry.Register(plugins.NewMetaPlugin(registry))
	s := New(
		"127.0.0.1",
		0,
		authToken,
		[]string{"http://127.0.0.1"},
		store,
		runner,
		channels.NewRouter(),
		registry,
		dir,
	)
	return s
}

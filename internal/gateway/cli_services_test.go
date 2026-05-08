package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLogsEndpoint(t *testing.T) {
	s := buildTestServer(t, "")
	s.logs.Append("info", "test", "hello", nil)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /logs: %d", resp.StatusCode)
	}
	var result struct {
		Logs []map[string]any `json:"logs"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(result.Logs))
	}
}

func TestCronEndpoint(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	// add a job
	body := `{"id":"j1","name":"j1","schedule":"@every 1h","command":"echo hi","enabled":true}`
	resp, err := http.Post(ts.URL+"/cron", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /cron: %d", resp.StatusCode)
	}

	// list
	resp, err = http.Get(ts.URL + "/cron")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		Jobs []map[string]any `json:"jobs"`
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &result)
	if len(result.Jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(result.Jobs))
	}
}

func TestSecretsEndpoint(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"name":"mykey","value":"myvalue"}`
	resp, err := http.Post(ts.URL+"/secrets", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /secrets: %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/secrets")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		Secrets []map[string]any `json:"secrets"`
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &result)
	if len(result.Secrets) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(result.Secrets))
	}
}

func TestHooksEndpoint(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"id":"h1","name":"h1","event":"message.received","type":"log","enabled":true}`
	resp, err := http.Post(ts.URL+"/hooks", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /hooks: %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/hooks")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		Hooks []map[string]any `json:"hooks"`
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &result)
	if len(result.Hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(result.Hooks))
	}
}

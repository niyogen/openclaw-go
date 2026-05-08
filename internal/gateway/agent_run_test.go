package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentRunEndpoint(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"sessionId":"run-test","message":"hello agent"}`
	resp, err := http.Post(ts.URL+"/agent/run", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /agent/run: status %d body %s", resp.StatusCode, raw)
	}
	var result struct {
		Reply string `json:"reply"`
		Turns int    `json:"turns"`
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Reply == "" {
		t.Fatal("expected non-empty reply")
	}
	if result.Turns < 1 {
		t.Fatalf("expected >=1 turn, got %d", result.Turns)
	}
}

func TestApprovalListEmpty(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/approvals")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /approvals: %d", resp.StatusCode)
	}
}

func TestRPCAgentRun(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"jsonrpc":"2.0","id":99,"method":"agent.run","params":{"sessionId":"rpc-run","message":"ping"}}`
	resp, err := http.Post(ts.URL+"/rpc", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var envelope struct {
		Result map[string]any `json:"result"`
		Error  *rpcError      `json:"error"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error != nil {
		t.Fatalf("rpc error: %+v", envelope.Error)
	}
	if envelope.Result["reply"] == "" {
		t.Fatal("expected non-empty reply in result")
	}
}

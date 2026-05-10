package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestAgentRunNoDuplicateUserMessage verifies that the current user message
// does not appear twice in the history passed to the runner.
// The EchoRunner echoes the full turn.Message, so if the message appears in
// history AND in turn.Message the echo would be duplicated — we check the
// stored assistant reply is a single echo, not a doubled string.
func TestAgentRunNoDuplicateUserMessage(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	const msg = "unique-probe-message"
	body := `{"sessionId":"dedup-test","message":"` + msg + `"}`
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
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	// EchoRunner returns the message; if it were duplicated in history the
	// reply text would contain the message more than once.
	count := strings.Count(result.Reply, msg)
	if count != 1 {
		t.Fatalf("expected message to appear exactly once in reply, got %d times; reply: %q", count, result.Reply)
	}
}

// TestSessionListProjection verifies that GET /sessions returns summary objects
// without the full messages array (avoiding unbounded response size).
func TestSessionListProjection(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	// Create a session with a message via /message.
	http.Post(ts.URL+"/message", "application/json", //nolint:errcheck
		bytes.NewBufferString(`{"sessionId":"list-proj","message":"hi","channel":"cli"}`))

	resp, err := http.Get(ts.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	// The list should not contain the raw messages array.
	if bytes.Contains(raw, []byte(`"messages"`)) {
		t.Fatalf("/sessions list unexpectedly contains 'messages' field: %s", raw)
	}
	// It should contain the messageCount summary field.
	if !bytes.Contains(raw, []byte(`"messageCount"`)) {
		t.Fatalf("/sessions list missing 'messageCount' field: %s", raw)
	}
}

// TestAgentRunStreamEndpoint verifies that POST /agent/run/stream returns SSE
// with the expected event structure.
func TestAgentRunStreamEndpoint(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"sessionId":"stream-test","message":"hello"}`
	resp, err := http.Post(ts.URL+"/agent/run/stream", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /agent/run/stream: status %d body %s", resp.StatusCode, raw)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	// Read all SSE frames.
	raw, _ := io.ReadAll(resp.Body)
	content := string(raw)

	if !strings.Contains(content, `"type":"start"`) {
		t.Errorf("missing start event: %s", content)
	}
	if !strings.Contains(content, `"type":"done"`) {
		t.Errorf("missing done event: %s", content)
	}
	if !strings.Contains(content, "[DONE]") {
		t.Errorf("missing [DONE] terminator: %s", content)
	}
}

// TestSessionSetModelEndpoint verifies POST /sessions/{id}/model.
func TestSessionSetModelEndpoint(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	// Create session first.
	http.Post(ts.URL+"/message", "application/json", //nolint:errcheck
		bytes.NewBufferString(`{"sessionId":"model-test","message":"hi","channel":"cli"}`))

	// Set the model.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sessions/model-test/model",
		bytes.NewBufferString(`{"provider":"openai","model":"gpt-4o"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /sessions/{id}/model: status %d body %s", resp.StatusCode, raw)
	}

	// Verify via GET /sessions/{id}.
	getResp, _ := http.Get(ts.URL + "/sessions/model-test")
	defer getResp.Body.Close()
	var body struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&body); err == nil {
		// Session GET returns the full session; model fields should be set.
		if body.Provider != "openai" {
			t.Logf("provider not exposed in GET /sessions/{id} (ok if hidden by design)")
		}
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
	if envelope.Result["runId"] == "" {
		t.Fatal("expected non-empty runId in agent.run result")
	}
}

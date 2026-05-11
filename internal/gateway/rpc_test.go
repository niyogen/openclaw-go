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

func TestRPCGatewayStatus(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	body := `{"jsonrpc":"2.0","id":1,"method":"gateway.status","params":{}}`
	resp, err := http.Post(ts.URL+"/rpc", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var envelope struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Result["version"] != Version {
		t.Fatalf("expected version %q in result", Version)
	}
	ag, ok := envelope.Result["agent"].(map[string]any)
	if !ok || ag["provider"] != "echo" {
		t.Fatalf("expected agent.provider echo in gateway.status, got %#v", envelope.Result["agent"])
	}
}

func TestRPCSessionsGetAndDelete(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	_, err := http.Post(
		ts.URL+"/message",
		"application/json",
		bytes.NewBufferString(`{"sessionId":"rpc-seg","message":"x","channel":"cli"}`),
	)
	if err != nil {
		t.Fatal(err)
	}

	getBody := `{"jsonrpc":"2.0","id":2,"method":"sessions.get","params":{"sessionId":"rpc-seg"}}`
	resp, err := http.Post(ts.URL+"/rpc", "application/json", strings.NewReader(getBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	delBody := `{"jsonrpc":"2.0","id":3,"method":"sessions.delete","params":{"sessionId":"rpc-seg"}}`
	resp, err = http.Post(ts.URL+"/rpc", "application/json", strings.NewReader(delBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
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
	if envelope.Result["deleted"] != "rpc-seg" {
		t.Fatalf("unexpected result: %s", raw)
	}
}

func TestRPCToolsListAndInvoke(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	listBody := `{"jsonrpc":"2.0","id":11,"method":"tools.list","params":{}}`
	resp, err := http.Post(ts.URL+"/rpc", "application/json", strings.NewReader(listBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools.list returned %d", resp.StatusCode)
	}

	invokeBody := `{"jsonrpc":"2.0","id":12,"method":"tools.invoke","params":{"name":"echo","arguments":{"text":"hi"}}}`
	resp, err = http.Post(ts.URL+"/rpc", "application/json", strings.NewReader(invokeBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
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
}

func TestRESTSessionGetDelete(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	_, err := http.Post(
		ts.URL+"/message",
		"application/json",
		bytes.NewBufferString(`{"sessionId":"rest1","message":"hi","channel":"cli"}`),
	)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/sessions/rest1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET session: %d", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/sessions/rest1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE session: %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/sessions/rest1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", resp.StatusCode)
	}
}

func TestRESTToolsInvoke(t *testing.T) {
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/tools")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /tools: %d", resp.StatusCode)
	}

	payload := `{"name":"echo","arguments":{"text":"hello"}}`
	resp, err = http.Post(ts.URL+"/tools/invoke", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /tools/invoke: %d", resp.StatusCode)
	}
}

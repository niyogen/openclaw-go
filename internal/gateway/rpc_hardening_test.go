package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleRPC_ParseError(t *testing.T) {
	s := buildTestServer(t, "")
	h := withBodyLimit(s.handleRPC)
	req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(`not json`))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
	var out rpcResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Error == nil || out.Error.Code != -32700 {
		t.Fatalf("body %s", rec.Body.String())
	}
}

func TestHandleRPC_InvalidJsonRPCVersion(t *testing.T) {
	s := buildTestServer(t, "")
	h := withBodyLimit(s.handleRPC)
	body := `{"jsonrpc":"1.0","id":1,"method":"health"}`
	req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out rpcResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Error == nil || out.Error.Code != -32600 {
		t.Fatalf("body %s", rec.Body.String())
	}
}

func TestHandleRPC_WrongMethod(t *testing.T) {
	s := buildTestServer(t, "")
	h := withBodyLimit(s.handleRPC)
	req := httptest.NewRequest(http.MethodGet, "/rpc", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestHandleRPC_BodyExceedsLimit(t *testing.T) {
	s := buildTestServer(t, "")
	h := withBodyLimit(s.handleRPC)
	// Inflate a string field so total body exceeds maxBodyBytes.
	pad := strings.Repeat("x", maxBodyBytes+100)
	body := `{"jsonrpc":"2.0","id":1,"method":"health","params":{"p":"` + pad + `"}}`
	req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d body=%s", rec.Code, truncateStr(rec.Body.String(), 200))
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

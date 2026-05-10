package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openclaw-go/internal/topology"
)

func TestNodeInvokeForwardsToRemoteRPC(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rpc" {
			t.Fatalf("remote path %q", r.URL.Path)
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var req struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatal(err)
		}
		if req.Method != "health" {
			t.Fatalf("method %q", req.Method)
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true,"from":"remote"}}`))
	}))
	defer remote.Close()

	s := buildTestServer(t, "secret")
	if err := s.topo.AddNode(topology.Node{ID: "n-remote", Name: "r", URL: remote.URL}); err != nil {
		t.Fatal(err)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"node.invoke","params":{"nodeId":"n-remote","method":"health"}}`
	req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handleRPC(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var outer rpcResponse
	if err := json.NewDecoder(rec.Body).Decode(&outer); err != nil {
		t.Fatal(err)
	}
	if outer.Error != nil {
		t.Fatalf("rpc error: %+v", outer.Error)
	}
	rawRes, err := json.Marshal(outer.Result)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(rawRes, &m); err != nil {
		t.Fatal(err)
	}
	if m["from"] != "remote" {
		t.Fatalf("result %+v", m)
	}
}

func TestNodeInvokeInvalidNodeURLScheme(t *testing.T) {
	s := buildTestServer(t, "secret")
	_ = s.topo.AddNode(topology.Node{ID: "bad", Name: "b", URL: "ftp://example.com/rpc"})

	body := `{"jsonrpc":"2.0","id":1,"method":"node.invoke","params":{"nodeId":"bad","method":"health"}}`
	req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handleRPC(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var outer rpcResponse
	_ = json.NewDecoder(rec.Body).Decode(&outer)
	if outer.Error == nil {
		t.Fatalf("expected error body %s", rec.Body.String())
	}
}

func TestNodeInvokePropagatesRemoteRPCError(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"bad"}}`))
	}))
	defer remote.Close()

	s := buildTestServer(t, "secret")
	_ = s.topo.AddNode(topology.Node{ID: "n1", URL: remote.URL})

	body := `{"jsonrpc":"2.0","id":1,"method":"node.invoke","params":{"nodeId":"n1","method":"anything"}}`
	req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handleRPC(rec, req)
	var outer rpcResponse
	if err := json.NewDecoder(rec.Body).Decode(&outer); err != nil {
		t.Fatal(err)
	}
	if outer.Error == nil || outer.Error.Code != -32600 {
		t.Fatalf("body %s", rec.Body.String())
	}
}

func TestNodeInvokeSendsBearerFromNodeAPIKey(t *testing.T) {
	var auth string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer remote.Close()

	s := buildTestServer(t, "secret")
	_ = s.topo.AddNode(topology.Node{ID: "n2", URL: remote.URL, APIKey: "node-secret-xyz"})

	body := `{"jsonrpc":"2.0","id":1,"method":"node.invoke","params":{"nodeId":"n2","method":"health"}}`
	req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handleRPC(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if auth != "Bearer node-secret-xyz" {
		t.Fatalf("Authorization %q", auth)
	}
}

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"openclaw-go/internal/topology"
)

func TestForwardNodeRPC_NullParamsBecomesEmptyObject(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer srv.Close()

	node := topology.Node{URL: srv.URL}
	_, rpcErr := forwardNodeRPC(context.Background(), node, "health", json.RawMessage(`null`))
	if rpcErr != nil {
		t.Fatalf("rpc err %+v", rpcErr)
	}
	var outer struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &outer); err != nil {
		t.Fatal(err)
	}
	if string(outer.Params) != "{}" {
		t.Fatalf("params body %s", string(outer.Params))
	}
}

func TestForwardNodeRPC_RemoteJSONRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`))
	}))
	defer srv.Close()

	node := topology.Node{URL: srv.URL}
	_, rpcErr := forwardNodeRPC(context.Background(), node, "unknown.method", json.RawMessage(`{}`))
	if rpcErr == nil {
		t.Fatal("expected rpc error")
		return
	}
	if rpcErr.Code != -32601 {
		t.Fatalf("code %d", rpcErr.Code)
	}
}

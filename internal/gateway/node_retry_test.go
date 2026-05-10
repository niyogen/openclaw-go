package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"openclaw-go/internal/topology"
)

func TestForwardNodeRPCRetriesThenSucceeds(t *testing.T) {
	var n atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := n.Add(1)
		if c < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`busy`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer remote.Close()

	node := topology.Node{URL: remote.URL}
	res, rpcErr := forwardNodeRPC(context.Background(), node, "health", json.RawMessage(`{}`))
	if rpcErr != nil {
		t.Fatalf("%+v", rpcErr)
	}
	m, _ := res.(map[string]any)
	if m["ok"] != true {
		t.Fatalf("%+v", res)
	}
	if n.Load() != 3 {
		t.Fatalf("attempts %d", n.Load())
	}
}

func TestForwardNodeRPCNoRetryOn401(t *testing.T) {
	var n atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`no`))
	}))
	defer remote.Close()

	_, rpcErr := forwardNodeRPC(context.Background(), topology.Node{URL: remote.URL}, "health", json.RawMessage(`{}`))
	if rpcErr == nil {
		t.Fatal("expected error")
	}
	if n.Load() != 1 {
		t.Fatalf("attempts %d want 1", n.Load())
	}
}

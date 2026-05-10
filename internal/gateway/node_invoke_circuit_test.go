package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openclaw-go/internal/topology"
)

func TestNodeInvokeCircuitBreakerBlocksAfterFailures(t *testing.T) {
	var remoteHits atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		remoteHits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`busy`))
	}))
	defer remote.Close()

	s := buildTestServer(t, "secret")
	s.nodeBreakerReg = newNodeBreakerRegistry(nodeCircuitSettings{Threshold: 2, Cooldown: 5 * time.Minute})
	s.nodeInvokeStats = newNodeInvokeStatsRegistry()

	if err := s.topo.AddNode(topology.Node{ID: "cb-peer", Name: "x", URL: remote.URL}); err != nil {
		t.Fatal(err)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"node.invoke","params":{"nodeId":"cb-peer","method":"health"}}`
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.handleRPC(rec, req)
		return rec
	}

	for i := 0; i < 2; i++ {
		rec := do()
		if rec.Code != http.StatusOK {
			t.Fatalf("invoke %d status %d", i, rec.Code)
		}
		var outer rpcResponse
		if err := json.NewDecoder(rec.Body).Decode(&outer); err != nil {
			t.Fatal(err)
		}
		if outer.Error == nil {
			t.Fatalf("invoke %d expected rpc error", i)
		}
	}

	hAfterFail := remoteHits.Load()
	want := int32(2 * maxNodeRPCAttempts)
	if hAfterFail != want {
		t.Fatalf("remote hits %d want %d", hAfterFail, want)
	}

	rec := do()
	var outer rpcResponse
	if err := json.NewDecoder(rec.Body).Decode(&outer); err != nil {
		t.Fatal(err)
	}
	if outer.Error == nil || !strings.Contains(outer.Error.Message, "circuit open") {
		t.Fatalf("expected circuit error, got %+v body=%s", outer.Error, rec.Body.String())
	}
	if remoteHits.Load() != hAfterFail {
		t.Fatalf("expected no new remote hits, before %d after %d", hAfterFail, remoteHits.Load())
	}
}

func TestNodeInvokeMetricsExposeOutcomes(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer remote.Close()

	s := buildTestServer(t, "")
	_ = s.topo.AddNode(topology.Node{ID: "m1", URL: remote.URL})

	body := `{"jsonrpc":"2.0","id":1,"method":"node.invoke","params":{"nodeId":"m1","method":"health"}}`
	req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleRPC(rec, req)

	mreq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	mrec := httptest.NewRecorder()
	s.handleMetrics(mrec, mreq)
	out := mrec.Body.String()
	if !strings.Contains(out, `openclaw_node_invoke_calls_total{node="m1",result="success"}`) {
		t.Fatalf("missing success counter:\n%s", out)
	}
	if !strings.Contains(out, `openclaw_node_circuit_open{node="m1"}`) {
		t.Fatalf("missing circuit gauge:\n%s", out)
	}
}

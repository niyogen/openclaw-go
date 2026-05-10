package gateway

import (
	"context"
	"testing"
)

func TestDispatchRPCTracingStatus(t *testing.T) {
	s := buildTestServer(t, "")
	out, rpcErr := s.dispatchRPC(context.Background(), "tracing.status", nil)
	if rpcErr != nil {
		t.Fatalf("%+v", rpcErr)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("type %T", out)
	}
	if m["requestIdHeader"] != "X-Request-ID" {
		t.Fatalf("%+v", m)
	}
}

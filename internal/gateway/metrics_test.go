package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"openclaw-go/internal/agents"
	"openclaw-go/internal/channels"
	"openclaw-go/internal/plugins"
	"openclaw-go/internal/sessions"
)

func TestHandleMetrics_PrometheusText(t *testing.T) {
	dir := testDataDir(t)
	store, err := sessions.New(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry := plugins.NewRegistry()
	registry.Register(plugins.NewMetaPlugin(registry))
	s := New("127.0.0.1", 0, "", nil, store, &agents.EchoRunner{}, channels.NewRouter(), registry, dir)

	s.rpcCallsTotal.Add(2)
	s.channelInboundsTotal.Add(5)
	s.agentRunsTotal.Add(1)
	s.agentRunsFailedTotal.Add(1)
	s.channelDispatchErrTotal.Add(3)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, line := range []string{
		"openclaw_gateway_uptime_seconds",
		"openclaw_gateway_rpc_calls_total 2",
		"openclaw_gateway_channel_inbound_total 5",
		"openclaw_gateway_agent_runs_total 1",
		"openclaw_gateway_agent_runs_failed_total 1",
		"openclaw_gateway_channel_dispatch_errors_total 3",
	} {
		if !strings.Contains(body, line) {
			t.Fatalf("missing %q in:\n%s", line, body)
		}
	}
}

package gateway

import (
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"time"
)

// handleMetrics exposes a minimal Prometheus text exposition for operators
// (uptime, memory, gateway counters). By default it is unauthenticated; when
// SetMetricsRequireAuth(true) is set and gateway auth is configured, the same
// rules as withAuth apply (Bearer, X-OpenClaw-Token, ?token=, Basic, trusted proxy).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.metricsRequireAuth.Load() && !s.isAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	uptime := time.Since(s.startedAt).Seconds()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	fmt.Fprintf(w, "# HELP openclaw_gateway_uptime_seconds Seconds since the gateway process started.\n")
	fmt.Fprintf(w, "# TYPE openclaw_gateway_uptime_seconds gauge\n")
	fmt.Fprintf(w, "openclaw_gateway_uptime_seconds %g\n", uptime)

	fmt.Fprintf(w, "# HELP openclaw_gateway_memory_heap_inuse_bytes Bytes in in-use spans from the Go heap.\n")
	fmt.Fprintf(w, "# TYPE openclaw_gateway_memory_heap_inuse_bytes gauge\n")
	fmt.Fprintf(w, "openclaw_gateway_memory_heap_inuse_bytes %d\n", ms.HeapInuse)

	fmt.Fprintf(w, "# HELP openclaw_gateway_goroutines Number of live goroutines.\n")
	fmt.Fprintf(w, "# TYPE openclaw_gateway_goroutines gauge\n")
	fmt.Fprintf(w, "openclaw_gateway_goroutines %d\n", runtime.NumGoroutine())

	fmt.Fprintf(w, "# HELP openclaw_gateway_rpc_calls_total JSON-RPC requests accepted (after jsonrpc 2.0 validation).\n")
	fmt.Fprintf(w, "# TYPE openclaw_gateway_rpc_calls_total counter\n")
	fmt.Fprintf(w, "openclaw_gateway_rpc_calls_total %s\n", u64str(s.rpcCallsTotal.Load()))

	fmt.Fprintf(w, "# HELP openclaw_gateway_channel_inbound_total Inbound channel messages (HandleInbound).\n")
	fmt.Fprintf(w, "# TYPE openclaw_gateway_channel_inbound_total counter\n")
	fmt.Fprintf(w, "openclaw_gateway_channel_inbound_total %s\n", u64str(s.channelInboundsTotal.Load()))

	fmt.Fprintf(w, "# HELP openclaw_gateway_channel_inbound_errors_total Inbound dispatches where HandleInbound returned an error.\n")
	fmt.Fprintf(w, "# TYPE openclaw_gateway_channel_inbound_errors_total counter\n")
	fmt.Fprintf(w, "openclaw_gateway_channel_inbound_errors_total %s\n", u64str(s.channelInboundErrTotal.Load()))

	fmt.Fprintf(w, "# HELP openclaw_gateway_agent_runs_total Completed agent runs (blocking + stream).\n")
	fmt.Fprintf(w, "# TYPE openclaw_gateway_agent_runs_total counter\n")
	fmt.Fprintf(w, "openclaw_gateway_agent_runs_total %s\n", u64str(s.agentRunsTotal.Load()))

	fmt.Fprintf(w, "# HELP openclaw_gateway_agent_runs_failed_total Agent runs that finished with an error.\n")
	fmt.Fprintf(w, "# TYPE openclaw_gateway_agent_runs_failed_total counter\n")
	fmt.Fprintf(w, "openclaw_gateway_agent_runs_failed_total %s\n", u64str(s.agentRunsFailedTotal.Load()))

	fmt.Fprintf(w, "# HELP openclaw_gateway_channel_dispatch_errors_total Outbound channel dispatch failures after retries.\n")
	fmt.Fprintf(w, "# TYPE openclaw_gateway_channel_dispatch_errors_total counter\n")
	fmt.Fprintf(w, "openclaw_gateway_channel_dispatch_errors_total %s\n", u64str(s.channelDispatchErrTotal.Load()))

	writeNodeInvokeMetrics(w, s)
}

func u64str(v uint64) string {
	return strconv.FormatUint(v, 10)
}

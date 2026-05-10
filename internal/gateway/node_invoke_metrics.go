package gateway

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type nodeInvokeStats struct {
	success     atomic.Uint64
	failure     atomic.Uint64
	circuitOpen atomic.Uint64
	sumNanos    atomic.Uint64
	count       atomic.Uint64
}

type nodeInvokeStatsRegistry struct {
	mu    sync.Mutex
	stats map[string]*nodeInvokeStats
}

func newNodeInvokeStatsRegistry() *nodeInvokeStatsRegistry {
	return &nodeInvokeStatsRegistry{stats: make(map[string]*nodeInvokeStats)}
}

func (r *nodeInvokeStatsRegistry) record(nodeID, outcome string, d time.Duration) {
	if r == nil || nodeID == "" {
		return
	}
	r.mu.Lock()
	st, ok := r.stats[nodeID]
	if !ok {
		st = &nodeInvokeStats{}
		r.stats[nodeID] = st
	}
	r.mu.Unlock()

	switch outcome {
	case "success":
		st.success.Add(1)
	case "failure":
		st.failure.Add(1)
	case "circuit_open":
		st.circuitOpen.Add(1)
	default:
		return
	}
	if outcome == "success" || outcome == "failure" {
		st.sumNanos.Add(uint64(d.Nanoseconds()))
		st.count.Add(1)
	}
}

func promEscapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

func writeNodeInvokeMetrics(w io.Writer, s *Server) {
	if s == nil || s.nodeInvokeStats == nil {
		return
	}

	now := time.Now().UTC()
	var blocking map[string]bool
	if s.nodeBreakerReg != nil {
		blocking = s.nodeBreakerReg.snapshotStates(now)
	}

	idSet := make(map[string]struct{})
	s.nodeInvokeStats.mu.Lock()
	for id := range s.nodeInvokeStats.stats {
		idSet[id] = struct{}{}
	}
	s.nodeInvokeStats.mu.Unlock()
	for id := range blocking {
		idSet[id] = struct{}{}
	}
	if len(idSet) == 0 {
		return
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	fmt.Fprintf(w, "# HELP openclaw_node_invoke_calls_total Outcomes of node.invoke per peer (after validation).\n")
	fmt.Fprintf(w, "# TYPE openclaw_node_invoke_calls_total counter\n")
	fmt.Fprintf(w, "# HELP openclaw_node_invoke_duration_seconds_sum Wall time spent in node.invoke (success and failure only), seconds.\n")
	fmt.Fprintf(w, "# TYPE openclaw_node_invoke_duration_seconds_sum counter\n")
	fmt.Fprintf(w, "# HELP openclaw_node_invoke_duration_seconds_count node.invoke calls that completed with success or failure (not circuit_open).\n")
	fmt.Fprintf(w, "# TYPE openclaw_node_invoke_duration_seconds_count counter\n")
	fmt.Fprintf(w, "# HELP openclaw_node_circuit_open Whether the node.invoke circuit is open (rejecting calls) for this peer (1=open).\n")
	fmt.Fprintf(w, "# TYPE openclaw_node_circuit_open gauge\n")

	for _, id := range ids {
		lbl := promEscapeLabelValue(id)
		s.nodeInvokeStats.mu.Lock()
		st := s.nodeInvokeStats.stats[id]
		s.nodeInvokeStats.mu.Unlock()

		if st != nil {
			fmt.Fprintf(w, "openclaw_node_invoke_calls_total{node=%s,result=\"success\"} %s\n", lbl, u64str(st.success.Load()))
			fmt.Fprintf(w, "openclaw_node_invoke_calls_total{node=%s,result=\"failure\"} %s\n", lbl, u64str(st.failure.Load()))
			fmt.Fprintf(w, "openclaw_node_invoke_calls_total{node=%s,result=\"circuit_open\"} %s\n", lbl, u64str(st.circuitOpen.Load()))
			if cnt := st.count.Load(); cnt > 0 {
				sumSec := float64(st.sumNanos.Load()) / 1e9
				fmt.Fprintf(w, "openclaw_node_invoke_duration_seconds_sum{node=%s} %g\n", lbl, sumSec)
				fmt.Fprintf(w, "openclaw_node_invoke_duration_seconds_count{node=%s} %s\n", lbl, u64str(cnt))
			}
		}
		if blocking != nil {
			v := 0.0
			if blocking[id] {
				v = 1
			}
			fmt.Fprintf(w, "openclaw_node_circuit_open{node=%s} %g\n", lbl, v)
		}
	}
}

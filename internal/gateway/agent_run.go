package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"openclaw-go/internal/agents"
	"openclaw-go/internal/hookstore"
	"openclaw-go/internal/logstore"
	"openclaw-go/internal/runtime"
	"openclaw-go/internal/sessions"
)

// agentRunStore holds in-memory run results (keyed by run id).
// Runs are pruned after a TTL to avoid unbounded growth.
type agentRunStore struct {
	mu   sync.Mutex
	runs map[string]*agentRunEntry
}

type agentRunEntry struct {
	result    runtime.RunResult
	createdAt time.Time
}

var globalRunStore = &agentRunStore{runs: map[string]*agentRunEntry{}}

func (s *agentRunStore) get(id string) (*agentRunEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.runs[id]
	return e, ok
}

func (s *agentRunStore) put(id string, result runtime.RunResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[id] = &agentRunEntry{result: result, createdAt: time.Now().UTC()}
	// Prune old entries (>1h).
	for k, v := range s.runs {
		if time.Since(v.createdAt) > time.Hour {
			delete(s.runs, k)
		}
	}
}

type agentRunRequest struct {
	SessionID    string              `json:"sessionId"`
	Message      string              `json:"message"`
	Instructions string              `json:"instructions,omitempty"` // system prompt
	Policy       *runtime.ExecPolicy `json:"policy,omitempty"`
}

type agentRunResponse struct {
	RunID     string `json:"runId"`
	SessionID string `json:"sessionId"`
	Reply     string `json:"reply"`
	Turns     int    `json:"turns"`
	Error     string `json:"error,omitempty"`
}

func (s *Server) handleAgentRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req agentRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sessionId is required"})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}

	policy := runtime.DefaultPolicy()
	if req.Policy != nil {
		policy = *req.Policy
	}

	_ = s.store.UpsertSession(req.SessionID, "cli", "")

	// Snapshot history BEFORE appending the current user message so that
	// buildMessages (which also appends turn.Message) doesn't duplicate it.
	var history []agents.HistoryMessage
	if sess, ok := s.store.Get(req.SessionID); ok {
		for _, m := range sess.Messages {
			history = append(history, agents.HistoryMessage{
				Role:    string(m.Role),
				Content: m.Content,
			})
		}
	}

	_ = s.store.AppendMessage(req.SessionID, sessions.Message{
		Role:      sessions.RoleUser,
		Content:   req.Message,
		CreatedAt: time.Now().UTC(),
	})

	// Inherit server-wide context window default when request doesn't specify one.
	if policy.MaxContextMessages == 0 && s.defaultMaxContextMsgs > 0 {
		policy.MaxContextMessages = s.defaultMaxContextMsgs
	}

	toolFn := func(ctx context.Context, name string, args map[string]any) (any, error) {
		return s.tools.Invoke(ctx, ToolInvokeRequest{Name: name, Arguments: args})
	}
	exec := runtime.NewExecutor(s.runnerForSession(req.SessionID), toolFn)
	exec.SetSubagentFn(func(ctx context.Context, message, instructions string) (string, error) {
		// Run the sub-agent with its own isolated turn (no shared session history).
		// Pass instructions as a system message so the sub-agent has the right context.
		var subHistory []agents.HistoryMessage
		if strings.TrimSpace(instructions) != "" {
			subHistory = []agents.HistoryMessage{{Role: "system", Content: instructions}}
		}
		reply, err := s.runnerForSession(req.SessionID).GenerateReply(ctx, agents.Turn{Message: message, History: subHistory})
		if err != nil {
			return "", err
		}
		return reply, nil
	})
	result := exec.Run(r.Context(), runtime.RunOptions{
		SessionID:    req.SessionID,
		Message:      req.Message,
		History:      history,
		Instructions: req.Instructions,
		Policy:       policy,
		Approvals:    s.approvals,
	})

	var errStr string
	if result.Err != nil {
		errStr = result.Err.Error()
	} else if result.FinalText != "" {
		_ = s.store.AppendMessage(req.SessionID, sessions.Message{
			Role:      sessions.RoleAssistant,
			Content:   result.FinalText,
			CreatedAt: time.Now().UTC(),
		})
	}

	runID := generateRunID()
	globalRunStore.put(runID, result)

	s.hooks.Emit(hookstore.EventAgentRunComplete, map[string]any{
		"runId": runID, "sessionId": req.SessionID, "turns": len(result.Turns), "error": errStr,
	})
	s.appendLog(logstore.LevelInfo, "agent", "run complete: "+runID, map[string]any{"turns": len(result.Turns)})

	writeJSON(w, http.StatusOK, agentRunResponse{
		RunID:     runID,
		SessionID: req.SessionID,
		Reply:     result.FinalText,
		Turns:     len(result.Turns),
		Error:     errStr,
	})
}

// handleAgentRunStream is POST /agent/run/stream.
// It accepts the same body as /agent/run and streams RunEvents as SSE.
func (s *Server) handleAgentRunStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	var req agentRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sessionId is required"})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}

	policy := runtime.DefaultPolicy()
	if req.Policy != nil {
		policy = *req.Policy
	}

	_ = s.store.UpsertSession(req.SessionID, "cli", "")

	var history []agents.HistoryMessage
	if sess, ok2 := s.store.Get(req.SessionID); ok2 {
		for _, m := range sess.Messages {
			history = append(history, agents.HistoryMessage{
				Role:    string(m.Role),
				Content: m.Content,
			})
		}
	}

	_ = s.store.AppendMessage(req.SessionID, sessions.Message{
		Role:      sessions.RoleUser,
		Content:   req.Message,
		CreatedAt: time.Now().UTC(),
	})

	// Inherit server-wide context window default when request doesn't specify one.
	if policy.MaxContextMessages == 0 && s.defaultMaxContextMsgs > 0 {
		policy.MaxContextMessages = s.defaultMaxContextMsgs
	}

	toolFn := func(ctx context.Context, name string, args map[string]any) (any, error) {
		return s.tools.Invoke(ctx, ToolInvokeRequest{Name: name, Arguments: args})
	}
	exec := runtime.NewExecutor(s.runnerForSession(req.SessionID), toolFn)
	exec.SetSubagentFn(func(ctx context.Context, message, instructions string) (string, error) {
		var subHistory []agents.HistoryMessage
		if strings.TrimSpace(instructions) != "" {
			subHistory = []agents.HistoryMessage{{Role: "system", Content: instructions}}
		}
		reply, err := s.runnerForSession(req.SessionID).GenerateReply(ctx, agents.Turn{Message: message, History: subHistory})
		if err != nil {
			return "", err
		}
		return reply, nil
	})

	// Pre-generate runID so it is available even if the stream fails/is cancelled.
	streamRunID := generateRunID()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	// Expose the runId in a response header so the client can retrieve the
	// run result via GET /agent/run/{runId} even if the stream is interrupted.
	w.Header().Set("X-Run-Id", streamRunID)
	w.WriteHeader(http.StatusOK)

	writeSSE := func(ev runtime.RunEvent) {
		raw, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		flusher.Flush()
	}

	events := make(chan runtime.RunEvent, 32)
	go func() {
		exec.RunStream(r.Context(), runtime.RunOptions{
			SessionID:    req.SessionID,
			Message:      req.Message,
			History:      history,
			Instructions: req.Instructions,
			Policy:       policy,
			Approvals:    s.approvals,
		}, events)
	}()

	var finalReply string
	var turnCount int
	var errStr string

	for ev := range events {
		writeSSE(ev)
		switch ev.Type {
		case runtime.RunEventDone:
			finalReply = ev.Reply
			turnCount = ev.Turns
			// RunStream generates its own internal runID; we use our gateway runID.
		case runtime.RunEventError:
			errStr = ev.Error
		}
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	// Persist assistant reply and store run result (mirrors handleAgentRun behaviour).
	if errStr == "" && finalReply != "" {
		_ = s.store.AppendMessage(req.SessionID, sessions.Message{
			Role:      sessions.RoleAssistant,
			Content:   finalReply,
			CreatedAt: time.Now().UTC(),
		})
	}

	// Always store the run result so GET /agent/run/{runId} works for both
	// successful and failed streaming runs.
	result := runtime.RunResult{FinalText: finalReply}
	if errStr != "" {
		result.Err = fmt.Errorf("%s", errStr)
		result.FinalText = ""
	}
	for i := 0; i < turnCount; i++ {
		result.Turns = append(result.Turns, runtime.TurnResult{Turn: i + 1})
	}
	globalRunStore.put(streamRunID, result)

	s.hooks.Emit(hookstore.EventAgentRunComplete, map[string]any{
		"runId": streamRunID, "sessionId": req.SessionID, "turns": turnCount, "error": errStr,
	})
	s.appendLog(logstore.LevelInfo, "agent", "stream run complete: "+streamRunID, map[string]any{"turns": turnCount}) //nolint:errcheck
}

func (s *Server) handleApprovalsList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"approvals": s.approvals.List(),
	})
}

type approvalDecisionRequest struct {
	Approved bool `json:"approved"`
}

func (s *Server) handleApprovalDecide(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "approval id is required"})
		return
	}
	var req approvalDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if err := s.approvals.Decide(id, req.Approved); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "approved": req.Approved})
}

// handleAgentRunGet retrieves a stored run result by runId.
func (s *Server) handleAgentRunGet(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("runId"))
	if runID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "runId is required"})
		return
	}
	entry, ok := globalRunStore.get(runID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}
	errStr := ""
	if entry.result.Err != nil {
		errStr = entry.result.Err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runId":     runID,
		"reply":     entry.result.FinalText,
		"turns":     len(entry.result.Turns),
		"error":     errStr,
		"createdAt": entry.createdAt,
	})
}

// handleBulkDeleteSessions deletes sessions matching criteria.
// Body: {"olderThan":"24h"} or {"ids":["a","b"]}.
func (s *Server) handleBulkDeleteSessions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OlderThan string   `json:"olderThan"` // Go duration string e.g. "24h"
		IDs       []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	var removed int
	if len(req.IDs) > 0 {
		for _, id := range req.IDs {
			if ok, _ := s.store.Delete(id); ok {
				removed++
				s.bus.Publish(GatewayEvent{Type: EventSessionDeleted, SessionID: id})
				s.appendLog(logstore.LevelInfo, "sessions", "session deleted (bulk): "+id, nil) //nolint:errcheck
			}
		}
	} else if req.OlderThan != "" {
		dur, err := time.ParseDuration(req.OlderThan)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid olderThan duration: " + err.Error()})
			return
		}
		n, err := s.store.Cleanup(dur)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		removed = n
	} else {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provide ids or olderThan"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
}

var runIDMu sync.Mutex

func generateRunID() string {
	runIDMu.Lock()
	defer runIDMu.Unlock()
	return time.Now().UTC().Format("20060102-150405.999999999") + "-run"
}

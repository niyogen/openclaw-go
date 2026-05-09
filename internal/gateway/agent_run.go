package gateway

import (
	"context"
	"encoding/json"
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
	_ = s.store.AppendMessage(req.SessionID, sessions.Message{
		Role:      sessions.RoleUser,
		Content:   req.Message,
		CreatedAt: time.Now().UTC(),
	})

	var history []agents.HistoryMessage
	if sess, ok := s.store.Get(req.SessionID); ok {
		for _, m := range sess.Messages {
			history = append(history, agents.HistoryMessage{
				Role:    string(m.Role),
				Content: m.Content,
			})
		}
	}

	toolFn := func(ctx context.Context, name string, args map[string]any) (any, error) {
		return s.tools.Invoke(ctx, ToolInvokeRequest{Name: name, Arguments: args})
	}
	exec := runtime.NewExecutor(s.runner, toolFn)
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
	s.logs.Append(logstore.LevelInfo, "agent", "run complete: "+runID, map[string]any{"turns": len(result.Turns)})

	writeJSON(w, http.StatusOK, agentRunResponse{
		RunID:     runID,
		SessionID: req.SessionID,
		Reply:     result.FinalText,
		Turns:     len(result.Turns),
		Error:     errStr,
	})
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

var runIDMu sync.Mutex

func generateRunID() string {
	runIDMu.Lock()
	defer runIDMu.Unlock()
	return time.Now().UTC().Format("20060102-150405.999999999") + "-run"
}

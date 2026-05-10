package gateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"openclaw-go/internal/cronstore"
	"openclaw-go/internal/hookstore"
)

// ------------------------------------------------------------------
// Logs
// ------------------------------------------------------------------

func (s *Server) handleLogsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	level := q.Get("level")
	component := q.Get("component")
	limit := 100
	entries := s.logs.List(level, component, limit)
	writeJSON(w, http.StatusOK, map[string]any{"logs": entries})
}

// ------------------------------------------------------------------
// Cron
// ------------------------------------------------------------------

func (s *Server) handleCronList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.cron.List()})
}

func (s *Server) handleCronAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var job cronstore.Job
	if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if err := s.cron.Add(job); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": job.ID})
}

func (s *Server) handleCronDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "job id required"})
		return
	}
	deleted, err := s.cron.Remove(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !deleted {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": id})
}

// ------------------------------------------------------------------
// Hooks
// ------------------------------------------------------------------

func (s *Server) handleHooksList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"hooks": s.hooks.List()})
}

func (s *Server) handleHooksAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var hook hookstore.Hook
	if err := json.NewDecoder(r.Body).Decode(&hook); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if err := s.hooks.Add(hook); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": hook.ID})
}

func (s *Server) handleHooksDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hook id required"})
		return
	}
	deleted, err := s.hooks.Remove(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !deleted {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "hook not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": id})
}

// ------------------------------------------------------------------
// Secrets (metadata only via REST — values only via RPC)
// ------------------------------------------------------------------

func (s *Server) handleSecretsList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"secrets": s.secrets.List()})
}

func (s *Server) handleSecretsSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if err := s.secrets.Set(req.Name, req.Value); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": req.Name})
}

func (s *Server) handleSecretsDelete(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "secret name required"})
		return
	}
	deleted, err := s.secrets.Delete(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !deleted {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "secret not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": name})
}

// ------------------------------------------------------------------
// RPC dispatch helpers (called from dispatchRPC in server.go)
// ------------------------------------------------------------------

func (s *Server) rpcLogs(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Level     string `json:"level"`
		Component string `json:"component"`
		Limit     int    `json:"limit"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
	}
	if p.Limit <= 0 {
		p.Limit = 100
	}
	return map[string]any{"logs": s.logs.List(p.Level, p.Component, p.Limit)}, nil
}

func (s *Server) rpcCronList() (any, *rpcError) {
	return map[string]any{"jobs": s.cron.List()}, nil
}

func (s *Server) rpcCronAdd(params json.RawMessage) (any, *rpcError) {
	var job cronstore.Job
	if len(params) == 0 {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	if err := json.Unmarshal(params, &job); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	if err := s.cron.Add(job); err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	return map[string]any{"ok": true, "id": job.ID}, nil
}

func (s *Server) rpcCronDelete(params json.RawMessage) (any, *rpcError) {
	var p struct {
		ID string `json:"id"`
	}
	if len(params) == 0 {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.ID) == "" {
		return nil, &rpcError{Code: -32602, Message: "id is required"}
	}
	deleted, err := s.cron.Remove(p.ID)
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	if !deleted {
		return nil, &rpcError{Code: -32001, Message: "job not found"}
	}
	return map[string]any{"ok": true, "deleted": p.ID}, nil
}

func (s *Server) rpcHooksList() (any, *rpcError) {
	return map[string]any{"hooks": s.hooks.List()}, nil
}

func (s *Server) rpcHooksAdd(params json.RawMessage) (any, *rpcError) {
	var hook hookstore.Hook
	if len(params) == 0 {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	if err := json.Unmarshal(params, &hook); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	if err := s.hooks.Add(hook); err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	return map[string]any{"ok": true, "id": hook.ID}, nil
}

func (s *Server) rpcSecretsSet(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if len(params) == 0 {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.Name) == "" {
		return nil, &rpcError{Code: -32602, Message: "name is required"}
	}
	if err := s.secrets.Set(p.Name, p.Value); err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	return map[string]any{"ok": true, "name": p.Name}, nil
}

func (s *Server) rpcSecretsGet(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name string `json:"name"`
	}
	if len(params) == 0 {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.Name) == "" {
		return nil, &rpcError{Code: -32602, Message: "name is required"}
	}
	val, err := s.secrets.Get(p.Name)
	if err != nil {
		return nil, &rpcError{Code: -32001, Message: err.Error()}
	}
	return map[string]any{"name": p.Name, "value": val}, nil
}

func (s *Server) rpcSecretsList() (any, *rpcError) {
	return map[string]any{"secrets": s.secrets.List()}, nil
}

func (s *Server) rpcSecretsDelete(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name string `json:"name"`
	}
	if len(params) == 0 {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.Name) == "" {
		return nil, &rpcError{Code: -32602, Message: "name is required"}
	}
	deleted, err := s.secrets.Delete(p.Name)
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	if !deleted {
		return nil, &rpcError{Code: -32001, Message: "secret not found"}
	}
	return map[string]any{"ok": true, "deleted": p.Name}, nil
}

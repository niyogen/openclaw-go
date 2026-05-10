package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"openclaw-go/internal/agents"
	"openclaw-go/internal/config"
	"openclaw-go/internal/logstore"
	"openclaw-go/internal/sessions"
)

var skillHTTPClient = &http.Client{Timeout: 60 * time.Second}

// ApplyExtensionTools reloads config-driven skill and MCP tools (prefixes skill. / mcp.).
func (s *Server) ApplyExtensionTools(cfg config.Config) {
	s.extMu.Lock()
	s.skillCfg = append([]config.SkillConfig(nil), cfg.Skills...)
	s.mcpCfg = append([]config.MCPServerConfig(nil), cfg.MCP...)
	s.extMu.Unlock()

	s.tools.UnregisterByPrefix("skill.")
	s.tools.UnregisterByPrefix("mcp.")

	for _, sk := range cfg.Skills {
		if !sk.Enabled {
			continue
		}
		name := strings.TrimSpace(sk.Name)
		ep := strings.TrimSpace(sk.Endpoint)
		if name == "" || ep == "" {
			continue
		}
		toolName := "skill." + sanitizeToolSegment(name)
		desc := strings.TrimSpace(sk.Description)
		if desc == "" {
			desc = "HTTP skill: " + name
		}
		endpoint := ep
		skillName := name
		s.tools.Register(
			Tool{
				Name:        toolName,
				Description: desc,
				Parameters: &ToolParameters{
					Type: "object",
					Properties: map[string]ToolParameter{
						"arguments": {Type: "object", Description: "Arguments forwarded to the skill endpoint as JSON"},
					},
				},
			},
			func(ctx context.Context, args map[string]any) (any, error) {
				return invokeSkillEndpoint(ctx, endpoint, skillName, args)
			},
		)
	}

	for _, m := range cfg.MCP {
		if !m.Enabled {
			continue
		}
		if strings.TrimSpace(m.URL) == "" {
			continue
		}
		if err := s.registerMCPServerTools(m); err != nil {
			s.appendLog(logstore.LevelWarn, "mcp", "register failed", map[string]any{
				"server": m.Name,
				"error":  err.Error(),
			})
		}
	}
}

func invokeSkillEndpoint(ctx context.Context, endpoint, skillName string, args map[string]any) (any, error) {
	payloadArgs := args
	if sub, ok := args["arguments"].(map[string]any); ok {
		payloadArgs = sub
	}
	payload := map[string]any{
		"skill":     skillName,
		"arguments": payloadArgs,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := skillHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("skill endpoint HTTP %d: %s", resp.StatusCode, mcpTruncateErr(string(body), 512))
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return map[string]any{"raw": string(body)}, nil
	}
	return parsed, nil
}

// SetMemoryCompaction configures optional LLM summarization after agent runs.
func (s *Server) SetMemoryCompaction(m config.MemoryConfig) {
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	s.memoryCompactAfter = m.CompactAfter
	s.memorySummarize = m.SummarizeOnCompact
}

func (s *Server) memoryCompactionConfig() (compactAfter int, summarize bool) {
	s.memoryMu.RLock()
	defer s.memoryMu.RUnlock()
	return s.memoryCompactAfter, s.memorySummarize
}

func (s *Server) snapshotSkills() []config.SkillConfig {
	s.extMu.RLock()
	defer s.extMu.RUnlock()
	out := make([]config.SkillConfig, len(s.skillCfg))
	copy(out, s.skillCfg)
	return out
}

func (s *Server) maintainSessionMemory(ctx context.Context, sessionID string) {
	th, sum := s.memoryCompactionConfig()
	if th <= 0 || !sum {
		return
	}
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return
	}
	sess, ok := s.store.Get(sid)
	if !ok || len(sess.Messages) <= th {
		return
	}

	drop := len(sess.Messages) - th
	dropped := append([]sessions.Message(nil), sess.Messages[:drop]...)
	keep := append([]sessions.Message(nil), sess.Messages[drop:]...)

	var b strings.Builder
	for _, m := range dropped {
		line := strings.TrimSpace(m.Content)
		if line == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", m.Role, line)
	}
	transcript := strings.TrimSpace(b.String())
	if transcript == "" {
		_, _ = s.store.Compact(sid, th)
		return
	}

	prompt := "Summarize the following conversation excerpt in 2–5 short sentences for use as memory context. Be factual; omit pleasantries.\n\n" + transcript
	reply, err := s.runnerForSession(sid).GenerateReply(ctx, agents.Turn{
		Message: prompt,
		History: nil,
	})
	if err != nil || strings.TrimSpace(reply) == "" {
		_, _ = s.store.Compact(sid, th)
		return
	}
	summary := sessions.Message{
		Role:      sessions.RoleSystem,
		Type:      sessions.MessageTypeText,
		Content:   "[Memory summary] " + strings.TrimSpace(reply),
		CreatedAt: time.Now().UTC(),
	}
	newMsgs := append([]sessions.Message{summary}, keep...)
	_ = s.store.SetMessages(sid, newMsgs)
}

// SkillsStatus returns configured skills for JSON-RPC skills.status.
func (s *Server) SkillsStatus() map[string]any {
	skills := s.snapshotSkills()
	list := make([]any, 0)
	n := 0
	for _, sk := range skills {
		if !sk.Enabled {
			continue
		}
		if strings.TrimSpace(sk.Name) == "" || strings.TrimSpace(sk.Endpoint) == "" {
			continue
		}
		n++
		list = append(list, map[string]any{
			"name":        sk.Name,
			"description": sk.Description,
			"endpoint":    sk.Endpoint,
			"toolName":    "skill." + sanitizeToolSegment(sk.Name),
		})
	}
	msg := "skills registered as gateway tools (skill.*)"
	if n == 0 {
		msg = "no enabled skills with name+endpoint in openclaw.json"
	}
	return map[string]any{"skills": list, "count": n, "message": msg}
}

// SkillsSearch filters skill metadata by query (JSON-RPC skills.search).
func (s *Server) SkillsSearch(params json.RawMessage) map[string]any {
	var p struct {
		Query string `json:"query"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	q := strings.ToLower(strings.TrimSpace(p.Query))
	base := s.SkillsStatus()
	raw, ok := base["skills"].([]any)
	if !ok {
		raw = []any{}
	}
	if q == "" {
		return map[string]any{"results": raw, "query": p.Query}
	}
	var out []any
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		desc, _ := m["description"].(string)
		if strings.Contains(strings.ToLower(name), q) || strings.Contains(strings.ToLower(desc), q) {
			out = append(out, m)
		}
	}
	return map[string]any{"results": out, "query": p.Query}
}

// SkillsDetail returns one skill by name (JSON-RPC skills.detail).
func (s *Server) SkillsDetail(params json.RawMessage) map[string]any {
	var p struct {
		Name string `json:"name"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	want := strings.TrimSpace(p.Name)
	for _, sk := range s.snapshotSkills() {
		if !sk.Enabled || strings.TrimSpace(sk.Name) != want {
			continue
		}
		return map[string]any{
			"name":        sk.Name,
			"description": sk.Description,
			"endpoint":    sk.Endpoint,
			"toolName":    "skill." + sanitizeToolSegment(sk.Name),
			"status":      "ok",
		}
	}
	return map[string]any{
		"name":   want,
		"status": "not_found",
		"note":   "no matching enabled skill in config",
	}
}

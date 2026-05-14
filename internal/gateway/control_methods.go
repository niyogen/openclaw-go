package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"openclaw-go/internal/agents"
	"openclaw-go/internal/config"
	"openclaw-go/internal/fileutil"
)

// This file holds the method-translation adapters that bridge the
// upstream openclaw WS protocol (used by openclaw-studio and other
// upstream-compatible frontends) to openclaw-go's native gateway
// surface. Protocol-layer concerns (handshake, framing, dispatch
// table itself) live in control_ws.go; method adapters live here so
// the table can grow without bloating the protocol code.
//
// Each adapter has two responsibilities:
//   1. Translate request params from upstream shape → openclaw-go shape.
//   2. Translate the response from openclaw-go shape → upstream shape.
//
// Where shapes match, the adapter is thin. Where they don't, the
// adapter does the legwork so the rest of the codebase stays clean.

// ── config.* ─────────────────────────────────────────────────────────

// configGetPayload is the upstream-shape response studio's adapter
// expects for `config.get`. The `config` field is the raw config-file
// contents (parsed as a JSON object); `hash` is a deterministic
// digest used by `config.patch` for optimistic-concurrency writes.
type configGetPayload struct {
	Config map[string]any `json:"config"`
	Hash   string         `json:"hash"`
	Exists bool           `json:"exists"`
	Path   string         `json:"path"`
}

// loadConfigForControl reads the config file at the resolved path
// (OPENCLAW_CONFIG_PATH override → ~/.openclaw-go/openclaw.json),
// parses it as a generic JSON object, and computes a stable hash.
// Returns the upstream-shape payload.
//
// Two failure modes:
//   - file does not exist → {config:{}, hash:"", exists:false, path}.
//     Studio treats this as the initial-setup case and is responsible
//     for sending a `config.set` to create the file.
//   - file exists but is unreadable / unparseable → return an rpcError
//     so studio surfaces a clear "config file is broken" message
//     rather than papering over it with an empty config.
func loadConfigForControl() (configGetPayload, *rpcError) {
	path, err := config.DefaultPath()
	if err != nil {
		return configGetPayload{}, &rpcError{Code: -32000, Message: "resolve config path: " + err.Error()}
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return configGetPayload{
			Config: map[string]any{},
			Hash:   "",
			Exists: false,
			Path:   path,
		}, nil
	}
	if err != nil {
		return configGetPayload{}, &rpcError{Code: -32000, Message: "read config: " + err.Error()}
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return configGetPayload{}, &rpcError{Code: -32603, Message: "parse config: " + err.Error()}
	}
	// Hash the file bytes (not re-encoded JSON) so the hash matches
	// what's actually on disk byte-for-byte. studio's config.patch
	// flow round-trips this hash; matching disk bytes is the most
	// defensible invariant.
	sum := sha256.Sum256(raw)
	return configGetPayload{
		Config: parsed,
		Hash:   hex.EncodeToString(sum[:]),
		Exists: true,
		Path:   path,
	}, nil
}

func handleConfigGet(_ *Server, ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	_ = ctx
	payload, rpcErr := loadConfigForControl()
	if rpcErr != nil {
		return nil, rpcErr
	}
	return payload, nil
}

// configPatchParams is the upstream-shape input studio sends for
// `config.patch`. `raw` is the full new config-file content as a JSON
// string; `baseHash` is the hash studio read via config.get and is
// used to detect concurrent edits — if our current hash differs we
// reject with a structured error studio retries on after re-reading.
type configPatchParams struct {
	Raw      string `json:"raw"`
	BaseHash string `json:"baseHash"`
}

func handleConfigPatch(_ *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	_ = ctx
	var p configPatchParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid config.patch params: " + err.Error()}
		}
	}
	if strings.TrimSpace(p.Raw) == "" {
		return nil, &rpcError{Code: -32602, Message: "config.patch: raw is required"}
	}
	// Validate the supplied raw is parseable before touching disk.
	var verify map[string]any
	if err := json.Unmarshal([]byte(p.Raw), &verify); err != nil {
		return nil, &rpcError{Code: -32602, Message: "config.patch: raw is not valid JSON: " + err.Error()}
	}
	// Optimistic-concurrency check: only enforce baseHash when the
	// file already exists. First-write case (file missing) is allowed
	// without a hash so studio's onboarding path doesn't get stuck.
	current, rpcErr := loadConfigForControl()
	if rpcErr != nil {
		return nil, rpcErr
	}
	if current.Exists {
		want := strings.TrimSpace(p.BaseHash)
		if want == "" {
			return nil, &rpcError{Code: -32602, Message: "config.patch: baseHash required to overwrite existing config; re-run config.get"}
		}
		if want != current.Hash {
			return nil, &rpcError{Code: -32001, Message: "config.patch: config changed since last load; re-run config.get"}
		}
	}
	// Write atomically at 0o600 (config may carry secrets).
	if err := fileutil.WriteFile(current.Path, []byte(p.Raw), 0o600); err != nil {
		return nil, &rpcError{Code: -32000, Message: "write config: " + err.Error()}
	}
	// Return the fresh hash so the caller can chain patches without
	// re-reading.
	sum := sha256.Sum256([]byte(p.Raw))
	return map[string]any{
		"ok":   true,
		"hash": hex.EncodeToString(sum[:]),
		"path": current.Path,
	}, nil
}

// handleConfigSet is a thin alias for `config.patch` minus the hash
// check — studio's gateway-permissions flow calls config.set when it
// intends a full overwrite without coordinating with concurrent
// editors. Same param shape (`raw`) but baseHash is ignored.
func handleConfigSet(_ *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	_ = ctx
	var p configPatchParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if strings.TrimSpace(p.Raw) == "" {
		return nil, &rpcError{Code: -32602, Message: "config.set: raw is required"}
	}
	var verify map[string]any
	if err := json.Unmarshal([]byte(p.Raw), &verify); err != nil {
		return nil, &rpcError{Code: -32602, Message: "config.set: raw is not valid JSON: " + err.Error()}
	}
	path, err := config.DefaultPath()
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: "resolve config path: " + err.Error()}
	}
	if err := fileutil.WriteFile(path, []byte(p.Raw), 0o600); err != nil {
		return nil, &rpcError{Code: -32000, Message: "write config: " + err.Error()}
	}
	sum := sha256.Sum256([]byte(p.Raw))
	return map[string]any{
		"ok":   true,
		"hash": hex.EncodeToString(sum[:]),
		"path": path,
	}, nil
}

// ── status (heartbeat-style snapshot) ──────────────────────────────

// handleStatus synthesizes the upstream-shape status response:
//
//	{ heartbeat: { agents: [{ agentId, enabled, every }, ...] } }
//
// Studio reads this for its heartbeat panel. openclaw-go doesn't yet
// have per-agent heartbeat metadata, so we return all configured
// agents with enabled:false and a default cadence. When a real
// heartbeat subsystem lands, this synthesis becomes a thin wrapper
// around its snapshot.
func handleStatus(s *Server, ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	_ = ctx
	list := s.workspace.List()
	heartbeats := make([]map[string]any, 0, len(list))
	for _, a := range list {
		heartbeats = append(heartbeats, map[string]any{
			"agentId": a.ID,
			"enabled": false,
			"every":   "30m",
		})
	}
	return map[string]any{
		"heartbeat": map[string]any{"agents": heartbeats},
		"gateway": map[string]any{
			"address":     s.Address(),
			"version":     Version,
			"authEnabled": s.authEnabledSnapshot(),
		},
	}, nil
}

// ── exec.approvals.* (stub) ────────────────────────────────────────

// handleExecApprovalsGet returns an empty exec-approvals snapshot in
// the shape studio expects:
//
//	{ file: { agents: {} } }
//
// openclaw-go doesn't currently persist per-agent exec security
// policies; this stub keeps studio's UI from breaking while the real
// feature is designed. set is a no-op.
func handleExecApprovalsGet(_ *Server, ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	_ = ctx
	return map[string]any{
		"file": map[string]any{
			"agents": map[string]any{},
		},
	}, nil
}

func handleExecApprovalsSet(_ *Server, ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	_ = ctx
	return map[string]any{"ok": true}, nil
}

// ── sessions.* ─────────────────────────────────────────────────────

// handleSessionsList adapts our `{sessions:[summary]}` response into
// the upstream shape, which uses `.key` as the primary identifier
// (we use `.id`). studio's hydration reads .key to scope agents to
// sessions, so the rename is load-bearing.
func handleSessionsList(s *Server, ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	_ = ctx
	all := s.store.List()
	out := make([]map[string]any, 0, len(all))
	for _, sess := range all {
		entry := map[string]any{
			"key":       sess.ID, // upstream's primary identifier
			"id":        sess.ID, // keep both so non-upstream clients still work
			"updatedAt": sess.UpdatedAt.UnixMilli(),
		}
		if sess.Channel != "" {
			entry["origin"] = map[string]any{"label": sess.Channel}
		}
		if sess.Provider != "" {
			entry["modelProvider"] = sess.Provider
		}
		if sess.Model != "" {
			entry["model"] = sess.Model
		}
		out = append(out, entry)
	}
	return map[string]any{"sessions": out}, nil
}

// handleSessionsPreview returns a preview window for each session
// key passed in. studio's bootstrap calls this with up to 64 keys to
// populate the fleet view. Params: { keys: [string], limit: int,
// maxChars: int }.
type sessionsPreviewParams struct {
	Keys     []string `json:"keys"`
	Limit    int      `json:"limit"`
	MaxChars int      `json:"maxChars"`
}

func handleSessionsPreview(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	_ = ctx
	var p sessionsPreviewParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if p.Limit <= 0 {
		p.Limit = 5
	}
	if p.MaxChars <= 0 {
		p.MaxChars = 280
	}
	// Upstream shape: previews is an ARRAY of {key, items: [{role,
	// text, timestamp}]}. Studio's preview-route iterates the array
	// looking for the matching key and reads .items — getting the
	// shape right is load-bearing for the chat panel to render
	// agent replies.
	previews := make([]map[string]any, 0, len(p.Keys))
	for _, key := range p.Keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		sess, ok := s.store.Get(key)
		if !ok {
			// Studio tolerates missing keys (no entry in the array
			// for that key); skip rather than fabricate.
			continue
		}
		hist := sess.Messages
		start := 0
		if len(hist) > p.Limit {
			start = len(hist) - p.Limit
		}
		items := make([]map[string]any, 0, len(hist)-start)
		for _, m := range hist[start:] {
			text := m.Content
			if len(text) > p.MaxChars {
				text = text[:p.MaxChars] + "…"
			}
			items = append(items, map[string]any{
				"role":      string(m.Role),
				"text":      text,
				"timestamp": m.CreatedAt.UnixMilli(),
			})
		}
		previews = append(previews, map[string]any{
			"key":   key,
			"items": items,
		})
	}
	return map[string]any{"previews": previews}, nil
}

// handleSessionsReset clears a session's message history while
// preserving the session id (so the UI doesn't lose its current
// selection). Different from sessions.delete which removes the
// session entirely.
type sessionsResetParams struct {
	Key       string `json:"key"`
	SessionID string `json:"sessionId"`
}

func handleSessionsReset(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	_ = ctx
	var p sessionsResetParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	id := strings.TrimSpace(p.Key)
	if id == "" {
		id = strings.TrimSpace(p.SessionID)
	}
	if id == "" {
		return nil, &rpcError{Code: -32602, Message: "sessions.reset: key or sessionId required"}
	}
	if _, ok := s.store.Get(id); !ok {
		return nil, &rpcError{Code: -32001, Message: "session not found: " + id}
	}
	if err := s.store.Reset(id); err != nil {
		return nil, &rpcError{Code: -32000, Message: "sessions.reset: " + err.Error()}
	}
	return map[string]any{"ok": true, "key": id}, nil
}

// handleSessionsPatch translates upstream's per-session-settings
// patch shape ({key, model?, thinkingLevel?, execHost?, ...}) to our
// native operations. Our native sessions.patch is message-history-
// focused; the upstream shape covers session-level settings.
//
// Each known field maps to whatever native handler we have. Unknown
// fields are silently accepted so a forward-compatible studio doesn't
// fail when we don't yet model a field. Returns ok=true on success
// even if no field was applicable (no-op).
type sessionsPatchParams struct {
	Key           string  `json:"key"`
	SessionID     string  `json:"sessionId"`
	Model         *string `json:"model"`
	Provider      *string `json:"provider"`
	ThinkingLevel *string `json:"thinkingLevel"`
	ExecHost      *string `json:"execHost"`
	ExecSecurity  *string `json:"execSecurity"`
	ExecAsk       *string `json:"execAsk"`
}

func handleSessionsPatch(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	_ = ctx
	var p sessionsPatchParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid sessions.patch params: " + err.Error()}
		}
	}
	id := strings.TrimSpace(p.Key)
	if id == "" {
		id = strings.TrimSpace(p.SessionID)
	}
	if id == "" {
		return nil, &rpcError{Code: -32602, Message: "sessions.patch: key or sessionId required"}
	}
	// Auto-upsert the session if it doesn't exist yet. Studio fires
	// sessions.patch on initial session-settings sync — which may
	// happen BEFORE the user has sent their first message — so a
	// "not found" error here would cascade as the agent's reply.
	// UpsertSession is idempotent for existing sessions.
	if _, ok := s.store.Get(id); !ok {
		if err := s.store.UpsertSession(id, "chat", ""); err != nil {
			return nil, &rpcError{Code: -32000, Message: "sessions.patch upsert: " + err.Error()}
		}
	}
	// Apply model/provider override when present. SetSessionModel
	// silently no-ops on empty strings so passing one without the
	// other is safe.
	if p.Model != nil || p.Provider != nil {
		provider := ""
		if p.Provider != nil {
			provider = strings.TrimSpace(*p.Provider)
		}
		model := ""
		if p.Model != nil {
			model = strings.TrimSpace(*p.Model)
		}
		if err := s.store.SetSessionModel(id, provider, model); err != nil {
			return nil, &rpcError{Code: -32000, Message: "sessions.patch model: " + err.Error()}
		}
	}
	// thinkingLevel / execHost / execSecurity / execAsk: openclaw-go
	// doesn't yet model these per-session. Silently accept and report
	// success so studio's UI doesn't error on settings it sets
	// optimistically. When the corresponding subsystems land, these
	// branches gain real persistence.
	return map[string]any{"ok": true, "key": id}, nil
}

// ── cron.* ─────────────────────────────────────────────────────────

func handleCronList(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	return s.dispatchRPC(ctx, "cron.list", params)
}

func handleCronAdd(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	return s.dispatchRPC(ctx, "cron.add", params)
}

func handleCronRun(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	return s.dispatchRPC(ctx, "cron.run", params)
}

// handleCronRemove aliases the upstream `cron.remove` to our native
// `cron.delete`. The param shape matches ({id}); only the method
// name differs.
func handleCronRemove(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	return s.dispatchRPC(ctx, "cron.delete", params)
}

// ── models.list ─────────────────────────────────────────────────────

func handleModelsList(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	return s.dispatchRPC(ctx, "models.list", params)
}

// ── chat.* ─────────────────────────────────────────────────────────

// chatParams is the upstream-shape input. Upstream uses sessionKey
// where openclaw-go natively uses sessionId; we accept either spelling
// for forward-compat.
type chatParams struct {
	SessionKey     string `json:"sessionKey"`
	SessionID      string `json:"sessionId"`
	Message        string `json:"message"`
	Channel        string `json:"channel"`
	IdempotencyKey string `json:"idempotencyKey"`
	Deliver        bool   `json:"deliver"`
}

func (p chatParams) resolveSessionID() string {
	if v := strings.TrimSpace(p.SessionKey); v != "" {
		return v
	}
	return strings.TrimSpace(p.SessionID)
}

// rebuildChatParams converts upstream chatParams to the JSON shape
// openclaw-go's native chat handlers expect (sessionId-keyed). Used
// by all three adapters below to keep the rename in one place.
func rebuildChatParams(p chatParams, fallbackChannel string) (json.RawMessage, *rpcError) {
	sid := p.resolveSessionID()
	if sid == "" {
		return nil, &rpcError{Code: -32602, Message: "sessionKey is required"}
	}
	ch := strings.TrimSpace(p.Channel)
	if ch == "" {
		ch = fallbackChannel
	}
	out := map[string]any{
		"sessionId": sid,
		"message":   p.Message,
	}
	if ch != "" {
		out["channel"] = ch
	}
	raw, _ := json.Marshal(out)
	return raw, nil
}

func handleChatSend(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p chatParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid chat.send params: " + err.Error()}
		}
	}
	rebuilt, rpcErr := rebuildChatParams(p, "chat")
	if rpcErr != nil {
		return nil, rpcErr
	}
	res, err := s.dispatchRPC(ctx, "chat.send", rebuilt)
	if err != nil {
		return nil, err
	}
	// Echo sessionKey back so studio's response handlers find the
	// expected field (upstream keys responses by sessionKey).
	if m, ok := res.(map[string]any); ok {
		m["sessionKey"] = p.resolveSessionID()
		return m, nil
	}
	return res, nil
}

// chatHistoryParams is the upstream-shape input for chat.history,
// including the limit field studio sends.
type chatHistoryParams struct {
	chatParams
	Limit int `json:"limit"`
}

func handleChatHistory(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p chatHistoryParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid chat.history params: " + err.Error()}
		}
	}
	sid := p.resolveSessionID()
	if sid == "" {
		return nil, &rpcError{Code: -32602, Message: "sessionKey is required"}
	}
	// Studio's chat.history caller expects `{messages: [...]}` where
	// each message has at minimum role + content/text. Our native
	// handler returns `{history: [...]}`; remap on the way out.
	limit := p.Limit
	if limit <= 0 {
		limit = 200
	}
	raw, _ := json.Marshal(map[string]any{"sessionId": sid, "limit": limit})
	res, rpcErr := s.dispatchRPC(ctx, "chat.history", raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	m, ok := res.(map[string]any)
	if !ok {
		return res, nil
	}
	// Rename `history` → `messages` to match upstream expectation;
	// preserve sessionKey/sessionId for the caller's debug log.
	if hist, ok := m["history"]; ok {
		m["messages"] = hist
	}
	if _, ok := m["messages"]; !ok {
		m["messages"] = []any{}
	}
	m["sessionKey"] = sid
	return m, nil
}

func handleChatAbort(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p chatParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid chat.abort params: " + err.Error()}
		}
	}
	sid := p.resolveSessionID()
	if sid == "" {
		return nil, &rpcError{Code: -32602, Message: "sessionKey is required"}
	}
	raw, _ := json.Marshal(map[string]any{"sessionId": sid})
	return s.dispatchRPC(ctx, "chat.abort", raw)
}

// ── agent.wait ─────────────────────────────────────────────────────

func handleAgentWait(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	return s.dispatchRPC(ctx, "agent.wait", params)
}

// ── agents.* (param-rename adapters) ─────────────────────────────

// upstream sends `{agentId, name, ...}` for agent CRUD; our native
// handlers want `{id, name, ...}` (AgentProfile.ID). The renaming
// adapters below rewrite the field before delegating.

type agentsUpsertParams struct {
	AgentID      string `json:"agentId"`
	ID           string `json:"id"` // accept either spelling
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	MaxTurns     int    `json:"maxTurns"`
}

func (p agentsUpsertParams) resolveID() string {
	if v := strings.TrimSpace(p.AgentID); v != "" {
		return v
	}
	return strings.TrimSpace(p.ID)
}

func (p agentsUpsertParams) toProfile() agents.AgentProfile {
	return agents.AgentProfile{
		ID:           p.resolveID(),
		Name:         p.Name,
		Description:  p.Description,
		Instructions: p.Instructions,
		Provider:     p.Provider,
		Model:        p.Model,
		MaxTurns:     p.MaxTurns,
	}
}

func handleAgentsCreate(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	_ = ctx
	var p agentsUpsertParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid agents.create params: " + err.Error()}
		}
	}
	profile := p.toProfile()
	if err := s.workspace.Create(profile); err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	return map[string]any{"ok": true, "id": profile.ID, "agentId": profile.ID}, nil
}

func handleAgentsUpdate(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	_ = ctx
	var p agentsUpsertParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid agents.update params: " + err.Error()}
		}
	}
	id := p.resolveID()
	if id == "" {
		return nil, &rpcError{Code: -32602, Message: "agents.update: id/agentId required"}
	}
	existing, ok := s.workspace.Get(id)
	if !ok {
		return nil, &rpcError{Code: -32001, Message: "agent not found: " + id}
	}
	// Partial update: only overwrite fields the caller supplied
	// (non-zero). Keeps the upstream `rename` operation (which sends
	// only name) from blanking out other fields.
	merged := existing
	if v := strings.TrimSpace(p.Name); v != "" {
		merged.Name = v
	}
	if v := strings.TrimSpace(p.Description); v != "" {
		merged.Description = v
	}
	if v := strings.TrimSpace(p.Instructions); v != "" {
		merged.Instructions = v
	}
	if v := strings.TrimSpace(p.Provider); v != "" {
		merged.Provider = v
	}
	if v := strings.TrimSpace(p.Model); v != "" {
		merged.Model = v
	}
	if p.MaxTurns > 0 {
		merged.MaxTurns = p.MaxTurns
	}
	if err := s.workspace.Update(merged); err != nil {
		return nil, &rpcError{Code: -32001, Message: err.Error()}
	}
	return map[string]any{"ok": true, "id": merged.ID, "agentId": merged.ID}, nil
}

func handleAgentsDelete(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	_ = ctx
	var p agentsUpsertParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	id := p.resolveID()
	if id == "" {
		return nil, &rpcError{Code: -32602, Message: "agents.delete: id/agentId required"}
	}
	deleted, err := s.workspace.Delete(id)
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	if !deleted {
		return nil, &rpcError{Code: -32001, Message: "agent not found: " + id}
	}
	return map[string]any{
		"ok":              true,
		"deleted":         id,
		"removedBindings": 0,
	}, nil
}

// handleAgentsFilesGet adapts the upstream shape (single-file fetch)
// to our existing native list — studio calls it with {agentId, path}
// and expects the matching file's content. We list, then filter.
type agentsFilesGetParams struct {
	AgentID string `json:"agentId"`
	Path    string `json:"path"`
}

func handleAgentsFilesGet(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	_ = ctx
	var p agentsFilesGetParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	id := strings.TrimSpace(p.AgentID)
	if id == "" {
		return nil, &rpcError{Code: -32602, Message: "agents.files.get: agentId required"}
	}
	files := s.workspace.ListArtifacts(id)
	wantPath := strings.TrimSpace(p.Path)
	for _, f := range files {
		if wantPath == "" || f.Name == wantPath || f.ID == wantPath {
			return map[string]any{
				"agentId": id,
				"path":    f.Name,
				"name":    f.Name,
				"content": f.Content,
				"type":    f.Type,
			}, nil
		}
	}
	return map[string]any{"agentId": id, "path": wantPath, "content": ""}, nil
}

// handleAgentsFilesSet writes a workspace artifact for an agent.
// Studio sends { agentId, path, content }. We mirror that as a
// workspace artifact with Name=path so retrieval via
// handleAgentsFilesGet round-trips.
type agentsFilesSetParams struct {
	AgentID string `json:"agentId"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Type    string `json:"type"`
}

func handleAgentsFilesSet(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	_ = ctx
	var p agentsFilesSetParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	id := strings.TrimSpace(p.AgentID)
	if id == "" {
		return nil, &rpcError{Code: -32602, Message: "agents.files.set: agentId required"}
	}
	if strings.TrimSpace(p.Path) == "" {
		return nil, &rpcError{Code: -32602, Message: "agents.files.set: path required"}
	}
	t := p.Type
	if t == "" {
		t = "text"
	}
	art := agents.Artifact{
		Name:    p.Path,
		Type:    t,
		Content: p.Content,
		AgentID: id,
	}
	if err := s.workspace.AddArtifact(art); err != nil {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}
	return map[string]any{"ok": true, "agentId": id, "path": p.Path}, nil
}

// ── exec.approval.resolve (stub) ───────────────────────────────────

// handleExecApprovalResolve answers an exec-approval prompt by id.
// openclaw-go's existing approvals.decide handler covers this — we
// pass through. Param shape lines up: both expect { id, decision }
// (decision = "approve" | "reject").
func handleExecApprovalResolve(s *Server, ctx context.Context, params json.RawMessage) (any, *rpcError) {
	return s.dispatchRPC(ctx, "approvals.decide", params)
}

// ── server-pushed event fanout ───────────────────────────────────

// fanoutControlEvents subscribes to the gateway's internal event bus
// and forwards events to a single /control/ws client as upstream-
// shape `{type: "event", event, payload, seq}` frames. Runs as a
// goroutine for the lifetime of a connected client; exits when the
// request context cancels (HTTP handler returns) or the bus channel
// closes.
//
// Studio's runtime adapter pipes incoming events into a projection
// store; the browser SSE feed replays them. So without this fanout
// the UI never learns about new chat messages, runs, or session
// changes — the only path is full re-poll on user action.
//
// Translation rules (current Phase 2 scope — see
// translateGatewayEvent for the exact mapping):
//
//   - SessionMessage / AgentReply → upstream `presence` event,
//     which studio classifies as summary-refresh. Triggers studio
//     to refetch preview/history, surfacing new chat content.
//   - SessionCreated / SessionDeleted → upstream `sessions.created`
//     / `sessions.deleted` events.
//   - everything else → dropped.
//
// The fully correct shape for chat is a run lifecycle
// (`agent.run.started` → `chat` deltas → `chat` final with runId).
// That requires a run-tracker in openclaw-go (Phase 4). `presence`
// is a working approximation today; the page may need a reload to
// see the very latest assistant reply on tight timing windows.
func (s *Server) fanoutControlEvents(ctx context.Context, seq *int64, writeFrame func(controlFrame) error) {
	evCh, unsub := s.bus.Subscribe("")
	defer unsub()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-evCh:
			if !ok {
				return
			}
			eventName, payload := translateGatewayEvent(ev)
			if eventName == "" {
				continue
			}
			nextSeq := atomic.AddInt64(seq, 1)
			_ = writeFrame(controlFrame{
				Type:    "event",
				Event:   eventName,
				Seq:     &nextSeq,
				Payload: payload,
			})
		}
	}
}

// translateGatewayEvent converts an internal GatewayEvent into the
// upstream-shape (eventName, payload) pair studio's adapter expects.
// Returns ("", nil) when the event has no upstream analogue or is
// missing required fields — caller drops it.
//
// Studio's runtime-chat workflow is run-state-machine driven
// (agent.run.started → chat delta(s) → chat final), and openclaw-go
// doesn't yet emit those lifecycle markers. Until the run-tracking
// support lands (Phase 4 of the upstream-protocol work), we fall back
// to firing `presence` events on every session change. Studio
// classifies `presence` as "summary-refresh" — which triggers a fresh
// preview/history fetch — so the UI picks up new chat content even
// without correct run-id correlation.
//
// This is a working approximation; the right long-term answer is a
// proper run tracker. See PARITY.md.
func translateGatewayEvent(ev GatewayEvent) (string, any) {
	switch ev.Type {
	case EventSessionMessage:
		// Trigger studio's summary-refresh path. Payload carries the
		// session key so studio can prioritize the right agent.
		return "presence", map[string]any{
			"sessionKey": ev.SessionID,
			"at":         time.Now().UTC().Format(time.RFC3339Nano),
		}
	case EventAgentReply:
		return "presence", map[string]any{
			"sessionKey": ev.SessionID,
			"at":         time.Now().UTC().Format(time.RFC3339Nano),
		}
	case EventSessionCreated:
		return "sessions.created", map[string]any{"sessionKey": ev.SessionID}
	case EventSessionDeleted:
		return "sessions.deleted", map[string]any{"sessionKey": ev.SessionID}
	default:
		return "", nil
	}
}

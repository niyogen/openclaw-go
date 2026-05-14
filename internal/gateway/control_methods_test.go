package gateway

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openclaw-go/internal/sessions"
)

// Unit tests for the method-translation adapters in control_methods.go.
// These exercise each adapter end-to-end against a real test gateway,
// verifying the upstream-shape inputs/outputs studio expects. The
// adapters were originally driven by live studio bootstrap behavior;
// these tests pin the contracts so future refactors don't quietly
// break what studio depends on.

// newCtlForMethod builds a server, opens /control/ws, completes the
// connect handshake, and returns a function that drives method calls
// against that connection. Tests use this to focus on the method
// behavior without re-doing the handshake plumbing.
func newCtlForMethod(t *testing.T) (callFn func(id, method string, params any) controlFrame, srv *Server) {
	t.Helper()
	s := buildTestServer(t, "")
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	conn := connectFor(t, ts, "")
	call := func(id, method string, params any) controlFrame {
		writeReq(t, conn, id, method, params)
		return readFrame(t, conn)
	}
	return call, s
}

func TestControlWSStatusReturnsHeartbeatShape(t *testing.T) {
	// Studio expects {heartbeat: {agents: [...]}}; with no agents the
	// list is empty but the keys must be present so studio doesn't
	// crash on undefined access.
	call, _ := newCtlForMethod(t)
	f := call("s1", "status", map[string]any{})
	if f.OK == nil || !*f.OK {
		t.Fatalf("status failed: %+v", f.Error)
	}
	p, _ := f.Payload.(map[string]any)
	hb, ok := p["heartbeat"].(map[string]any)
	if !ok {
		t.Fatalf("expected heartbeat key in payload: %+v", p)
	}
	if _, ok := hb["agents"].([]any); !ok {
		t.Errorf("heartbeat.agents must be array, got %T", hb["agents"])
	}
	gw, _ := p["gateway"].(map[string]any)
	if gw["version"] == nil {
		t.Errorf("gateway.version should be present")
	}
}

func TestControlWSWakeIgnoresParamsButRespondsOK(t *testing.T) {
	// Studio sometimes calls wake({mode:"now", text:"trigger"}); we
	// accept any params and respond ok=true.
	call, _ := newCtlForMethod(t)
	f := call("w1", "wake", map[string]any{"mode": "now", "text": "trigger"})
	if f.OK == nil || !*f.OK {
		t.Fatalf("wake failed: %+v", f.Error)
	}
	p, _ := f.Payload.(map[string]any)
	if p["woke"] != true {
		t.Errorf("expected woke=true, got %+v", p)
	}
}

func TestControlWSConfigGetReturnsDiskShape(t *testing.T) {
	// Test layout: write a config to disk via OPENCLAW_CONFIG_PATH,
	// then verify config.get returns {config, hash, exists, path}
	// with the parsed contents and a deterministic hash.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openclaw.json")
	content := `{"gateway":{"port":12345,"host":"127.0.0.1"}}`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	t.Setenv("OPENCLAW_CONFIG_PATH", cfgPath)

	call, _ := newCtlForMethod(t)
	f := call("c1", "config.get", map[string]any{})
	if f.OK == nil || !*f.OK {
		t.Fatalf("config.get failed: %+v", f.Error)
	}
	p, _ := f.Payload.(map[string]any)
	if p["exists"] != true {
		t.Errorf("expected exists=true, got %v", p["exists"])
	}
	hash, _ := p["hash"].(string)
	if len(hash) != 64 {
		t.Errorf("expected 64-hex sha256, got %q", hash)
	}
	cfg, ok := p["config"].(map[string]any)
	if !ok {
		t.Fatalf("expected config map, got %T", p["config"])
	}
	gw, _ := cfg["gateway"].(map[string]any)
	if gw["port"].(float64) != 12345 {
		t.Errorf("expected gateway.port=12345, got %v", gw["port"])
	}
}

func TestControlWSConfigGetMissingFile(t *testing.T) {
	// Studio uses exists:false to decide it's in initial-setup mode.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "absent.json")
	t.Setenv("OPENCLAW_CONFIG_PATH", cfgPath)

	call, _ := newCtlForMethod(t)
	f := call("c1", "config.get", map[string]any{})
	if f.OK == nil || !*f.OK {
		t.Fatalf("config.get failed: %+v", f.Error)
	}
	p, _ := f.Payload.(map[string]any)
	if p["exists"] != false {
		t.Errorf("expected exists=false on missing file, got %v", p["exists"])
	}
	if p["hash"] != "" {
		t.Errorf("expected empty hash on missing file, got %v", p["hash"])
	}
}

func TestControlWSConfigPatchEnforcesBaseHash(t *testing.T) {
	// Optimistic concurrency: patch with wrong hash must fail; right
	// hash must succeed.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openclaw.json")
	if err := os.WriteFile(cfgPath, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	t.Setenv("OPENCLAW_CONFIG_PATH", cfgPath)

	call, _ := newCtlForMethod(t)
	// First read the current hash.
	f := call("c1", "config.get", map[string]any{})
	currentHash := f.Payload.(map[string]any)["hash"].(string)

	// Wrong hash: should reject.
	bad := call("p1", "config.patch", map[string]any{
		"raw":      `{"version":2}`,
		"baseHash": "deadbeef",
	})
	if bad.OK == nil || *bad.OK {
		t.Fatalf("expected wrong-hash patch to fail, got %+v", bad)
	}

	// Right hash: should succeed.
	good := call("p2", "config.patch", map[string]any{
		"raw":      `{"version":2}`,
		"baseHash": currentHash,
	})
	if good.OK == nil || !*good.OK {
		t.Fatalf("expected right-hash patch to succeed, got %+v", good.Error)
	}
	// File should now contain the new content.
	raw, _ := os.ReadFile(cfgPath)
	if string(raw) != `{"version":2}` {
		t.Errorf("disk content not updated, got %q", string(raw))
	}
}

func TestControlWSConfigPatchRejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openclaw.json")
	t.Setenv("OPENCLAW_CONFIG_PATH", cfgPath)

	call, _ := newCtlForMethod(t)
	f := call("p1", "config.patch", map[string]any{"raw": "not valid json"})
	if f.OK == nil || *f.OK {
		t.Fatalf("expected invalid-json patch to fail, got %+v", f)
	}
	if f.Error == nil || f.Error.Code != "INVALID_REQUEST" {
		t.Errorf("expected INVALID_REQUEST, got %+v", f.Error)
	}
}

func TestControlWSExecApprovalsStubsReturnEmptyShape(t *testing.T) {
	// Studio reads .file.agents — must exist or it'll fail .agents
	// access.
	call, _ := newCtlForMethod(t)
	f := call("e1", "exec.approvals.get", map[string]any{})
	if f.OK == nil || !*f.OK {
		t.Fatalf("exec.approvals.get failed: %+v", f.Error)
	}
	p, _ := f.Payload.(map[string]any)
	file, _ := p["file"].(map[string]any)
	if _, ok := file["agents"].(map[string]any); !ok {
		t.Errorf("expected file.agents map, got %T", file["agents"])
	}

	// set is a no-op, just check it returns ok=true.
	g := call("e2", "exec.approvals.set", map[string]any{"file": map[string]any{}})
	if g.OK == nil || !*g.OK {
		t.Errorf("exec.approvals.set should succeed: %+v", g.Error)
	}
}

func TestControlWSAgentsCreateAcceptsAgentIdAlias(t *testing.T) {
	// Studio sends `agentId`; our native handler reads `id`. Adapter
	// must accept the alias.
	call, _ := newCtlForMethod(t)
	f := call("a1", "agents.create", map[string]any{
		"agentId": "alpha-agent",
		"name":    "Alpha",
	})
	if f.OK == nil || !*f.OK {
		t.Fatalf("agents.create failed: %+v", f.Error)
	}
	p, _ := f.Payload.(map[string]any)
	if p["id"] != "alpha-agent" {
		t.Errorf("expected id=alpha-agent, got %v", p["id"])
	}
	// Studio reads agentId in the response too.
	if p["agentId"] != "alpha-agent" {
		t.Errorf("expected agentId=alpha-agent in response, got %v", p["agentId"])
	}
}

func TestControlWSAgentsUpdatePartialMerge(t *testing.T) {
	// Studio's rename-only flow sends just {agentId, name}; the
	// adapter must NOT blank out description/instructions/model.
	call, s := newCtlForMethod(t)
	// Seed: full profile.
	created := call("a0", "agents.create", map[string]any{
		"agentId":      "beta",
		"name":         "Beta",
		"description":  "original desc",
		"instructions": "original system prompt",
		"provider":     "echo",
		"model":        "echo-1",
	})
	if created.OK == nil || !*created.OK {
		t.Fatalf("seed agents.create failed: %+v", created.Error)
	}

	// Rename only.
	updated := call("a1", "agents.update", map[string]any{
		"agentId": "beta",
		"name":    "Beta Renamed",
	})
	if updated.OK == nil || !*updated.OK {
		t.Fatalf("agents.update failed: %+v", updated.Error)
	}

	// Verify other fields preserved.
	got, ok := s.workspace.Get("beta")
	if !ok {
		t.Fatalf("agent disappeared after update")
	}
	if got.Name != "Beta Renamed" {
		t.Errorf("expected name='Beta Renamed', got %q", got.Name)
	}
	if got.Description != "original desc" {
		t.Errorf("description was blanked: %q", got.Description)
	}
	if got.Instructions != "original system prompt" {
		t.Errorf("instructions were blanked: %q", got.Instructions)
	}
	if got.Provider != "echo" || got.Model != "echo-1" {
		t.Errorf("model/provider were blanked: %s/%s", got.Provider, got.Model)
	}
}

func TestControlWSAgentsDeleteAcceptsAgentIdAlias(t *testing.T) {
	call, _ := newCtlForMethod(t)
	_ = call("a0", "agents.create", map[string]any{"agentId": "gamma", "name": "Gamma"})
	f := call("a1", "agents.delete", map[string]any{"agentId": "gamma"})
	if f.OK == nil || !*f.OK {
		t.Fatalf("agents.delete failed: %+v", f.Error)
	}
	p, _ := f.Payload.(map[string]any)
	if p["deleted"] != "gamma" {
		t.Errorf("expected deleted=gamma, got %v", p["deleted"])
	}
	// removedBindings shape required by studio:
	if _, ok := p["removedBindings"]; !ok {
		t.Errorf("expected removedBindings in response (studio shape)")
	}
}

func TestControlWSSessionsPatchAutoUpsertsAndAcceptsUnknownFields(t *testing.T) {
	// Studio fires sessions.patch BEFORE the user's first message, so
	// the adapter must auto-create the session. And unknown fields
	// (thinkingLevel, execHost, etc.) must be silently accepted so a
	// forward-compatible studio doesn't fail.
	call, s := newCtlForMethod(t)
	f := call("sp1", "sessions.patch", map[string]any{
		"key":           "agent:main:main",
		"model":         "echo",
		"provider":      "echo",
		"thinkingLevel": "high",   // unknown to openclaw-go
		"execHost":      "remote", // unknown to openclaw-go
	})
	if f.OK == nil || !*f.OK {
		t.Fatalf("sessions.patch failed: %+v", f.Error)
	}
	if _, ok := s.store.Get("agent:main:main"); !ok {
		t.Errorf("session was not auto-upserted")
	}
}

func TestControlWSSessionsPreviewShapeIsArrayWithItems(t *testing.T) {
	// Studio's preview-route iterates an ARRAY of {key, items}; if
	// we accidentally return a map keyed by sessionKey (a real bug
	// during development), studio fails to render any reply.
	call, s := newCtlForMethod(t)
	_ = s.store.UpsertSession("session-a", "chat", "")
	// Append a couple of messages.
	if err := writeAssistantMessage(s, "session-a", "first"); err != nil {
		t.Fatal(err)
	}
	if err := writeAssistantMessage(s, "session-a", "second"); err != nil {
		t.Fatal(err)
	}

	f := call("p1", "sessions.preview", map[string]any{
		"keys":     []string{"session-a", "session-missing"},
		"limit":    10,
		"maxChars": 100,
	})
	if f.OK == nil || !*f.OK {
		t.Fatalf("sessions.preview failed: %+v", f.Error)
	}
	p, _ := f.Payload.(map[string]any)
	previews, ok := p["previews"].([]any)
	if !ok {
		t.Fatalf("previews must be array; got %T", p["previews"])
	}
	// Only existing key returns an entry; missing keys are skipped.
	if len(previews) != 1 {
		t.Fatalf("expected 1 preview entry, got %d", len(previews))
	}
	entry, _ := previews[0].(map[string]any)
	if entry["key"] != "session-a" {
		t.Errorf("expected key=session-a, got %v", entry["key"])
	}
	items, _ := entry["items"].([]any)
	if len(items) != 2 {
		t.Errorf("expected 2 items, got %d", len(items))
	}
	first, _ := items[0].(map[string]any)
	for _, field := range []string{"role", "text", "timestamp"} {
		if _, ok := first[field]; !ok {
			t.Errorf("item missing required field %q: %+v", field, first)
		}
	}
}

func TestControlWSChatHistorySessionKeyToSessionIdRename(t *testing.T) {
	// Studio sends sessionKey; our native chat.history expects
	// sessionId. The adapter renames, and remaps the response
	// history→messages.
	call, s := newCtlForMethod(t)
	_ = s.store.UpsertSession("sx", "chat", "")
	if err := writeAssistantMessage(s, "sx", "alpha reply"); err != nil {
		t.Fatal(err)
	}

	f := call("h1", "chat.history", map[string]any{
		"sessionKey": "sx",
		"limit":      10,
	})
	if f.OK == nil || !*f.OK {
		t.Fatalf("chat.history failed: %+v", f.Error)
	}
	p, _ := f.Payload.(map[string]any)
	msgs, ok := p["messages"].([]any)
	if !ok {
		t.Fatalf("expected messages array, got %T", p["messages"])
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	// Echo sessionKey back so studio matches the response.
	if p["sessionKey"] != "sx" {
		t.Errorf("expected sessionKey=sx echoed back, got %v", p["sessionKey"])
	}
}

func TestControlWSChatSendRequiresSessionKey(t *testing.T) {
	call, _ := newCtlForMethod(t)
	f := call("cs1", "chat.send", map[string]any{"message": "hi"})
	if f.OK == nil || *f.OK {
		t.Fatalf("chat.send without sessionKey must fail, got %+v", f)
	}
	if f.Error == nil || !strings.Contains(f.Error.Message, "sessionKey") {
		t.Errorf("expected sessionKey-required error, got %+v", f.Error)
	}
}

func TestControlWSCronRemoveAliasesToCronDelete(t *testing.T) {
	// cron.remove is just an upstream-name alias for cron.delete.
	call, _ := newCtlForMethod(t)
	f := call("cr1", "cron.remove", map[string]any{"id": "nonexistent"})
	// Expect an error (job not found) — proves we reached the
	// native handler, not a METHOD_NOT_FOUND.
	if f.OK != nil && *f.OK {
		// some implementations may return ok=true on missing; the
		// important thing is the method exists.
		return
	}
	if f.Error == nil {
		t.Fatalf("expected some response, got %+v", f)
	}
	if f.Error.Code == "METHOD_NOT_FOUND" {
		t.Errorf("cron.remove should alias to cron.delete, not METHOD_NOT_FOUND")
	}
}

// writeAssistantMessage is a tiny helper for seeding sessions in
// adapter tests. Uses the package-private AppendMessage entry.
func writeAssistantMessage(s *Server, sessID, content string) error {
	return s.store.AppendMessage(sessID, sessions.Message{
		Role:      sessions.RoleAssistant,
		Content:   content,
		CreatedAt: time.Now().UTC(),
	})
}

// ── translateGatewayEvent direct tests ─────────────────────────────

func TestTranslateGatewayEventSessionMessageBecomesPresence(t *testing.T) {
	ev := GatewayEvent{
		Type:      EventSessionMessage,
		SessionID: "agent:main:main",
		Data:      map[string]any{"role": "assistant", "content": "hi"},
	}
	name, payload := translateGatewayEvent(ev)
	if name != "presence" {
		t.Errorf("expected presence event name, got %q", name)
	}
	p, _ := payload.(map[string]any)
	if p["sessionKey"] != "agent:main:main" {
		t.Errorf("sessionKey mismatch: %v", p["sessionKey"])
	}
}

func TestTranslateGatewayEventSessionCreated(t *testing.T) {
	ev := GatewayEvent{Type: EventSessionCreated, SessionID: "agent:foo:bar"}
	name, _ := translateGatewayEvent(ev)
	if name != "sessions.created" {
		t.Errorf("expected sessions.created, got %q", name)
	}
}

func TestTranslateGatewayEventUnknownDropped(t *testing.T) {
	ev := GatewayEvent{Type: EventToolInvoked, SessionID: "x"}
	name, _ := translateGatewayEvent(ev)
	if name != "" {
		t.Errorf("expected unknown event to be dropped, got %q", name)
	}
}

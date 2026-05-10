package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openclaw-go/internal/config"
	"openclaw-go/internal/sessions"
)

func TestApplyExtensionToolsSkillHTTP(t *testing.T) {
	var body string
	ep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{"ok":true,"echoed":1}`))
	}))
	defer ep.Close()

	s := buildTestServer(t, "")
	s.ApplyExtensionTools(config.Config{
		Skills: []config.SkillConfig{
			{Enabled: true, Name: "demo", Description: "d", Endpoint: ep.URL},
		},
	})

	out, err := s.tools.Invoke(context.Background(), ToolInvokeRequest{
		Name:      "skill.demo",
		Arguments: map[string]any{"arguments": map[string]any{"q": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"skill":"demo"`) {
		t.Fatalf("body %q", body)
	}
	m, _ := out.(map[string]any)
	if m["ok"] != true {
		t.Fatalf("%+v", out)
	}
}

func TestApplyExtensionToolsMCPHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
			ID     any    `json:"id"`
		}
		_ = json.Unmarshal(b, &req)
		switch req.Method {
		case "initialize":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"ping","description":"p","inputSchema":{"type":"object","properties":{"x":{"type":"string"}}}}]}}`))
		case "tools/call":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"pong"}]}}`))
		default:
			t.Fatalf("unknown method %s body=%s", req.Method, string(b))
		}
	}))
	defer srv.Close()

	s := buildTestServer(t, "")
	s.ApplyExtensionTools(config.Config{
		MCP: []config.MCPServerConfig{
			{Enabled: true, Name: "test", URL: srv.URL},
		},
	})

	out, err := s.tools.Invoke(context.Background(), ToolInvokeRequest{
		Name:      "mcp.test.ping",
		Arguments: map[string]any{"x": "a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := out.(map[string]any)
	if m["text"] != "pong" {
		t.Fatalf("%+v", out)
	}
}

func TestMaintainSessionMemorySummarize(t *testing.T) {
	s := buildTestServer(t, "")
	s.SetMemoryCompaction(config.MemoryConfig{CompactAfter: 2, SummarizeOnCompact: true})
	_ = s.store.UpsertSession("s1", "cli", "")
	_ = s.store.AppendMessage("s1", sessions.Message{Role: sessions.RoleUser, Content: "a", CreatedAt: time.Now().UTC()})
	_ = s.store.AppendMessage("s1", sessions.Message{Role: sessions.RoleAssistant, Content: "b", CreatedAt: time.Now().UTC()})
	_ = s.store.AppendMessage("s1", sessions.Message{Role: sessions.RoleUser, Content: "c", CreatedAt: time.Now().UTC()})

	s.maintainSessionMemory(context.Background(), "s1")
	sess, _ := s.store.Get("s1")
	if len(sess.Messages) < 2 {
		t.Fatalf("expected summary+retained, got %d msgs", len(sess.Messages))
	}
	if sess.Messages[0].Role != sessions.RoleSystem || !strings.Contains(sess.Messages[0].Content, "Memory summary") {
		t.Fatalf("first msg: %+v", sess.Messages[0])
	}
}

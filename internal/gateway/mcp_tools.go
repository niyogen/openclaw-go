package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"openclaw-go/internal/config"
)

var mcpToolHTTPClient = &http.Client{Timeout: 60 * time.Second}

var toolNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeToolSegment(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = toolNameSanitizer.ReplaceAllString(s, "_")
	if s == "" {
		return "x"
	}
	return s
}

type mcpConn struct {
	baseURL   string
	apiKey    string
	sessionID string
	idSeq     atomic.Int64
	client    *http.Client
}

func newMCPConn(baseURL, apiKey string) *mcpConn {
	return &mcpConn{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:  strings.TrimSpace(apiKey),
		client:  mcpToolHTTPClient,
	}
}

func (c *mcpConn) nextID() int64 {
	return c.idSeq.Add(1)
}

func (c *mcpConn) postRPC(ctx context.Context, payload any) (*http.Response, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	return c.client.Do(req)
}

func (c *mcpConn) captureSession(resp *http.Response) {
	if resp == nil {
		return
	}
	if sid := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); sid != "" {
		c.sessionID = sid
	}
}

func (c *mcpConn) notify(ctx context.Context, method string, params any) error {
	body := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}
	resp, err := c.postRPC(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	c.captureSession(resp)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mcp notify HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *mcpConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID()
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	resp, err := c.postRPC(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	c.captureSession(resp)
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mcp HTTP %d: %s", resp.StatusCode, mcpTruncateErr(string(respBody), 256))
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("mcp invalid json: %w", err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("mcp error %d: %s", env.Error.Code, env.Error.Message)
	}
	return env.Result, nil
}

func mcpTruncateErr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (s *Server) registerMCPServerTools(srv config.MCPServerConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	conn := newMCPConn(srv.URL, srv.APIKey)
	baseName := sanitizeToolSegment(srv.Name)
	if baseName == "" {
		baseName = "server"
	}

	_, _ = conn.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "openclaw-go",
			"version": Version,
		},
	})
	_ = conn.notify(ctx, "notifications/initialized", map[string]any{})

	rawList, err := conn.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return err
	}
	var listed struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rawList, &listed); err != nil {
		return fmt.Errorf("tools/list parse: %w", err)
	}

	for _, t := range listed.Tools {
		tname := strings.TrimSpace(t.Name)
		if tname == "" {
			continue
		}
		fqn := "mcp." + baseName + "." + sanitizeToolSegment(tname)
		desc := strings.TrimSpace(t.Description)
		params := mcpInputSchemaToParams(t.InputSchema)
		tool := Tool{Name: fqn, Description: desc, Parameters: params}
		toolName := tname
		s.tools.Register(tool, func(c context.Context, args map[string]any) (any, error) {
			res, err := conn.call(c, "tools/call", map[string]any{
				"name":      toolName,
				"arguments": args,
			})
			if err != nil {
				return nil, err
			}
			return normalizeMCPCallResult(res)
		})
	}
	return nil
}

func normalizeMCPCallResult(raw json.RawMessage) (any, error) {
	var wrap struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &wrap) == nil && len(wrap.Content) > 0 {
		var parts []string
		for _, c := range wrap.Content {
			if strings.TrimSpace(c.Text) != "" {
				parts = append(parts, c.Text)
			}
		}
		if len(parts) > 0 {
			return map[string]any{"text": strings.Join(parts, "\n")}, nil
		}
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return map[string]any{"raw": string(raw)}, nil
	}
	return generic, nil
}

func mcpInputSchemaToParams(raw json.RawMessage) *ToolParameters {
	if len(raw) == 0 {
		return nil
	}
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	if typ, _ := root["type"].(string); typ != "" && typ != "object" {
		return &ToolParameters{Type: "object"}
	}
	props, _ := root["properties"].(map[string]any)
	if len(props) == 0 {
		return &ToolParameters{Type: "object"}
	}
	out := &ToolParameters{
		Type:       "object",
		Properties: map[string]ToolParameter{},
	}
	for k, v := range props {
		pm := ToolParameter{Type: "string"}
		if m, ok := v.(map[string]any); ok {
			if t, _ := m["type"].(string); t != "" {
				pm.Type = t
			}
			if d, _ := m["description"].(string); d != "" {
				pm.Description = d
			}
		}
		out.Properties[k] = pm
	}
	if req, ok := root["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok && s != "" {
				out.Required = append(out.Required, s)
			}
		}
	}
	return out
}

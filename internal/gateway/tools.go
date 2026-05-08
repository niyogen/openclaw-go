package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type ToolInvokeRequest struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type ToolHandler func(context.Context, map[string]any) (any, error)

type ToolRegistry struct {
	tools    []Tool
	handlers map[string]ToolHandler
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools:    []Tool{},
		handlers: map[string]ToolHandler{},
	}
}

func (r *ToolRegistry) Register(tool Tool, handler ToolHandler) {
	name := strings.TrimSpace(tool.Name)
	if name == "" || handler == nil {
		return
	}
	tool.Name = name
	r.tools = append(r.tools, tool)
	r.handlers[name] = handler
}

func (r *ToolRegistry) List() []Tool {
	out := make([]Tool, len(r.tools))
	copy(out, r.tools)
	return out
}

func (r *ToolRegistry) Invoke(ctx context.Context, req ToolInvokeRequest) (any, error) {
	if r == nil {
		return nil, errors.New("tool registry not initialized")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errors.New("tool name is required")
	}
	handler, ok := r.handlers[name]
	if !ok {
		return nil, fmt.Errorf("tool %q not found", name)
	}
	if req.Arguments == nil {
		req.Arguments = map[string]any{}
	}
	return handler(ctx, req.Arguments)
}

func (s *Server) initTools() {
	s.tools = NewToolRegistry()

	s.tools.Register(
		Tool{
			Name:        "time.now",
			Description: "Return gateway UTC time and unix timestamp",
		},
		func(_ context.Context, _ map[string]any) (any, error) {
			now := time.Now().UTC()
			return map[string]any{
				"utc":  now.Format(time.RFC3339Nano),
				"unix": now.Unix(),
			}, nil
		},
	)

	s.tools.Register(
		Tool{
			Name:        "echo",
			Description: "Echo back provided text argument",
		},
		func(_ context.Context, args map[string]any) (any, error) {
			text, _ := args["text"].(string)
			return map[string]any{"text": text}, nil
		},
	)

	s.tools.Register(
		Tool{
			Name:        "sessions.count",
			Description: "Return count of sessions in store",
		},
		func(_ context.Context, _ map[string]any) (any, error) {
			return map[string]any{"count": len(s.store.List())}, nil
		},
	)
}

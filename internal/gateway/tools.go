package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ToolParameter describes one parameter in a tool's JSON Schema.
type ToolParameter struct {
	Type        string         `json:"type"`
	Description string         `json:"description,omitempty"`
	Enum        []string       `json:"enum,omitempty"`
	Properties  map[string]any `json:"properties,omitempty"`
	Required    []string       `json:"required,omitempty"`
}

// ToolParameters is the JSON Schema for a tool's input arguments.
type ToolParameters struct {
	Type       string                   `json:"type"` // "object"
	Properties map[string]ToolParameter `json:"properties,omitempty"`
	Required   []string                 `json:"required,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  *ToolParameters `json:"parameters,omitempty"`
}

type ToolWithSchema struct {
	Type     string `json:"type"` // "function"
	Function Tool   `json:"function"`
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

// RegisterPluginTools registers any tools declared by external plugins.
func (s *Server) RegisterPluginTools(toolName, description, endpoint string) {
	client := &http.Client{}
	s.tools.Register(Tool{Name: toolName, Description: description},
		func(ctx context.Context, args map[string]any) (any, error) {
			return callPluginTool(ctx, client, endpoint, args)
		},
	)
}

func callPluginTool(ctx context.Context, client *http.Client, endpoint string, args map[string]any) (any, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
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
			Parameters: &ToolParameters{
				Type: "object",
				Properties: map[string]ToolParameter{
					"text": {Type: "string", Description: "Text to echo back"},
				},
				Required: []string{"text"},
			},
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

	s.tools.Register(
		Tool{
			Name:        "sandbox.run",
			Description: "Run a shell command or script inside a Docker sandbox (network=none, resource-limited).",
			Parameters: &ToolParameters{
				Type: "object",
				Properties: map[string]ToolParameter{
					"script": {Type: "string", Description: "Shell script to execute"},
					"image":  {Type: "string", Description: "Docker image (default: alpine:3.19)"},
				},
				Required: []string{"script"},
			},
		},
		func(ctx context.Context, args map[string]any) (any, error) {
			return sandboxRunTool(ctx, args)
		},
	)

	s.tools.Register(
		Tool{
			Name:        "sandbox.available",
			Description: "Check whether Docker is available for sandbox execution.",
		},
		func(ctx context.Context, _ map[string]any) (any, error) {
			available := sandboxIsAvailable(ctx)
			return map[string]any{"available": available}, nil
		},
	)
}

// SandboxRunFn is the function signature for executing a script in sandbox.
type SandboxRunFn func(ctx context.Context, script string, opts SandboxOpts) (*SandboxResult, error)

// SandboxAvailableFn checks if Docker is reachable.
type SandboxAvailableFn func(ctx context.Context) bool

// SandboxOpts carries resource limits for a sandbox run.
// Mirrors sandbox.Options without importing that package here.
type SandboxOpts struct {
	Image      string
	Network    string
	MemoryMB   int
	CPUs       float64
	TimeoutSec int
	ReadOnly   bool
}

// SandboxResult holds the output of a sandbox run.
type SandboxResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

// sandboxRunTool and sandboxIsAvailable are thin wrappers; replaced by
// SetSandboxImpl when the real sandbox package is available.
var sandboxRunTool = func(ctx context.Context, args map[string]any) (any, error) {
	script, _ := args["script"].(string)
	if strings.TrimSpace(script) == "" {
		return nil, fmt.Errorf("script argument is required")
	}
	return map[string]any{
		"note":   "sandbox.run requires Docker; call SetSandboxImpl to enable",
		"script": script,
	}, nil
}

var sandboxIsAvailable = func(_ context.Context) bool { return false }

// SetSandboxFuncs wires the real sandbox implementation into the gateway tool
// registry.  Call this from main after the gateway is constructed.
func SetSandboxFuncs(
	runFn func(ctx context.Context, script string, opts interface{}) (*SandboxResult, error),
	availFn func(ctx context.Context) bool,
) {
	sandboxIsAvailable = availFn
	sandboxRunTool = func(ctx context.Context, args map[string]any) (any, error) {
		script, _ := args["script"].(string)
		if strings.TrimSpace(script) == "" {
			return nil, fmt.Errorf("script argument is required")
		}
		if !availFn(ctx) {
			return nil, fmt.Errorf("docker is not available; cannot run sandbox")
		}
		result, err := runFn(ctx, script, nil)
		if err != nil {
			return nil, err
		}
		return result, nil
	}
}

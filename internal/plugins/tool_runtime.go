package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"openclaw-go/internal/fileutil"
)

// ToolPluginRegistry tracks approved tool plugins + their tokens, mirroring
// ChannelPluginRegistry's shape. Tokens are persisted to a sidecar file
// (mode 0o600) so plugins don't have to re-handshake after gateway
// restart. Tokens are reserved for future plugin → gateway callbacks
// (streaming tool results, async completions); the gateway does NOT
// currently send the token when invoking a plugin tool — that's a
// gateway → plugin call, and plugins typically run on loopback.
//
// Threading model: constructed once at startup, then approve/revoke is
// called from RPC handlers (one at a time per request) and ApprovedManifests/
// LookupApprovedManifest is read under RLock during dispatch.
type ToolPluginRegistry struct {
	mu         sync.RWMutex
	tokensFile string
	tokens     map[string]string
	manifests  map[string]Manifest
}

// NewToolPluginRegistry constructs a registry rooted at the supplied
// plugins directory + tokens persistence path. Tokens file is created
// at mode 0o600 on first save. Empty pluginsDir => empty registry
// (operator hasn't dropped any plugin.json files yet).
func NewToolPluginRegistry(pluginsDir, tokensFile string) (*ToolPluginRegistry, error) {
	r := &ToolPluginRegistry{
		tokensFile: tokensFile,
		tokens:     map[string]string{},
		manifests:  map[string]Manifest{},
	}
	if err := r.loadTokens(); err != nil {
		return nil, err
	}
	if err := r.scanManifests(pluginsDir); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *ToolPluginRegistry) loadTokens() error {
	raw, err := os.ReadFile(r.tokensFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, &r.tokens)
}

func (r *ToolPluginRegistry) saveTokensLocked() error {
	raw, err := json.MarshalIndent(r.tokens, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.tokensFile), 0o755); err != nil {
		return err
	}
	return fileutil.WriteFile(r.tokensFile, raw, 0o600)
}

// scanManifests indexes manifests that declare at least one tool. A
// single plugin.json may declare BOTH a channel and tools; in that
// case it shows up in both the channel-plugin registry and this one,
// and the operator approves each capability independently. This
// matches the design doc's "per-capability approval" posture.
func (r *ToolPluginRegistry) scanManifests(pluginsDir string) error {
	if pluginsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifestPath := filepath.Join(pluginsDir, entry.Name(), "plugin.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if strings.TrimSpace(m.Name) == "" {
			m.Name = entry.Name()
		}
		if !m.HasToolPlugin() {
			continue
		}
		// Note: we deliberately do NOT run validateManifestURLs here.
		// Tool plugins follow the same convention as channel plugins —
		// they typically run as sidecars on loopback (127.0.0.1:port),
		// which the SSRF validator rejects. The legacy Loader.Load()
		// path still validates manifests for the route/tool-proxy flow.
		// Tool-plugin endpoints are gated by per-plugin approval
		// instead; an operator who approves a malicious manifest has
		// already authorised the call.
		r.manifests[m.Name] = m
	}
	return nil
}

// ToolPluginEntry is the operator-visible view of a registered plugin's
// tool surface. State is "approved" when a token has been issued.
type ToolPluginEntry struct {
	Name        string           `json:"name"`
	Tools       []ToolPluginTool `json:"tools"`
	Version     string           `json:"version,omitempty"`
	Description string           `json:"description,omitempty"`
	State       string           `json:"state"` // "pending" | "approved"
}

// ToolPluginTool is one tool entry within a plugin's surface.
type ToolPluginTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Endpoint    string `json:"endpoint"`
}

// List returns every tool-plugin manifest known to the registry,
// pending or approved. Order is map-iteration; callers that need a
// stable order should sort. (List is small — typically <10 plugins.)
func (r *ToolPluginRegistry) List() []ToolPluginEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ToolPluginEntry, 0, len(r.manifests))
	for name, m := range r.manifests {
		state := "pending"
		if _, ok := r.tokens[name]; ok {
			state = "approved"
		}
		tools := make([]ToolPluginTool, 0, len(m.Tools))
		for _, t := range m.Tools {
			if strings.TrimSpace(t.Name) == "" || strings.TrimSpace(t.Endpoint) == "" {
				continue
			}
			tools = append(tools, ToolPluginTool{
				Name:        t.Name,
				Description: t.Description,
				Endpoint:    t.Endpoint,
			})
		}
		out = append(out, ToolPluginEntry{
			Name:        name,
			Tools:       tools,
			Version:     m.Version,
			Description: m.Description,
			State:       state,
		})
	}
	return out
}

// Approve flips a plugin from pending → approved, generating a fresh
// bearer token. Idempotent — if already approved, returns the existing
// token. To rotate, Revoke first.
func (r *ToolPluginRegistry) Approve(name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.manifests[name]; !ok {
		return "", fmt.Errorf("plugin %q not found (no manifest with tools)", name)
	}
	if existing, ok := r.tokens[name]; ok {
		return existing, nil
	}
	token, err := generatePluginToken()
	if err != nil {
		return "", err
	}
	r.tokens[name] = token
	if err := r.saveTokensLocked(); err != nil {
		delete(r.tokens, name)
		return "", err
	}
	return token, nil
}

// Revoke removes a plugin's token. Idempotent.
func (r *ToolPluginRegistry) Revoke(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tokens[name]; !ok {
		return nil
	}
	delete(r.tokens, name)
	return r.saveTokensLocked()
}

// LookupApprovedManifest returns the manifest for an approved tool plugin,
// or (zero, false) if unknown / pending.
func (r *ToolPluginRegistry) LookupApprovedManifest(name string) (Manifest, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.tokens[name]; !ok {
		return Manifest{}, false
	}
	m, ok := r.manifests[name]
	return m, ok
}

// ApprovedManifests returns every approved tool-plugin manifest. Used
// during gateway init to register tool handlers with the ToolRegistry.
func (r *ToolPluginRegistry) ApprovedManifests() []Manifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Manifest, 0, len(r.tokens))
	for name := range r.tokens {
		if m, ok := r.manifests[name]; ok {
			out = append(out, m)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────
// PluginToolHandler: gateway-side handler that POSTs tool invocations
// to the plugin's HTTP endpoint and returns the result.
// ─────────────────────────────────────────────────────────────────────

// PluginToolHandler is the gateway-side closure that handles tool
// invocations by POSTing to a plugin's endpoint. Returned by
// NewPluginToolHandler. Decoupled from the gateway's ToolHandler type
// to avoid an import cycle — the gateway wraps this in its own
// ToolHandler closure at registration time.
type PluginToolHandler func(ctx context.Context, args map[string]any) (any, error)

// NewPluginToolHandler returns a handler that POSTs the given args to
// the plugin's tool endpoint as JSON, decodes the response, and returns
// the result. Errors are surfaced clearly:
//
//   - Plugin process down → "plugin tool %s: connect: %w" (transient,
//     caller may retry).
//   - Plugin 4xx → returned as a permanent error with body included.
//   - Plugin 5xx → returned as a transient-looking error with body.
//   - Response body shape: { "result": <any> } OR { "error": "<msg>" }.
//     Anything else returns the entire body as the result so plugin
//     authors aren't locked into a strict envelope.
func NewPluginToolHandler(endpoint string) PluginToolHandler {
	client := &http.Client{Timeout: 30 * time.Second}
	return func(ctx context.Context, args map[string]any) (any, error) {
		if args == nil {
			args = map[string]any{}
		}
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("plugin tool: POST %s: %w", endpoint, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("plugin tool: %d: %s", resp.StatusCode, string(body))
		}
		// Try to decode as { result, error } envelope first.
		var envelope struct {
			Result any    `json:"result"`
			Error  string `json:"error,omitempty"`
		}
		if err := json.Unmarshal(body, &envelope); err == nil {
			if envelope.Error != "" {
				return nil, errors.New(envelope.Error)
			}
			if envelope.Result != nil {
				return envelope.Result, nil
			}
		}
		// No envelope — return the raw decoded body as the result.
		var raw2 any
		if err := json.Unmarshal(body, &raw2); err == nil {
			return raw2, nil
		}
		// Not JSON — return the text.
		return string(body), nil
	}
}

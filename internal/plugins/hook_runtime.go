package plugins

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"openclaw-go/internal/fileutil"
	"openclaw-go/internal/hookstore"
)

// HookPluginRegistry tracks approved hook plugins + their tokens.
// Mirrors ChannelPluginRegistry / ToolPluginRegistry. Tokens are
// reserved for future verification of plugin → gateway callbacks (when
// hooks ever grow a response channel beyond fire-and-forget); the
// gateway currently does NOT send the token when delivering a hook
// envelope to a plugin endpoint.
type HookPluginRegistry struct {
	mu         sync.RWMutex
	tokensFile string
	tokens     map[string]string
	manifests  map[string]Manifest
}

// NewHookPluginRegistry constructs a registry rooted at the supplied
// plugins directory + tokens persistence path. Tokens file is created
// at mode 0o600 on first save.
func NewHookPluginRegistry(pluginsDir, tokensFile string) (*HookPluginRegistry, error) {
	r := &HookPluginRegistry{
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

func (r *HookPluginRegistry) loadTokens() error {
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

func (r *HookPluginRegistry) saveTokensLocked() error {
	raw, err := json.MarshalIndent(r.tokens, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.tokensFile), 0o755); err != nil {
		return err
	}
	return fileutil.WriteFile(r.tokensFile, raw, 0o600)
}

// scanManifests indexes manifests that declare at least one hook.
// As with the tool registry, we do NOT run validateManifestURLs —
// hook plugins are expected to run on loopback like every other
// plugin category. Security boundary is approval.
func (r *HookPluginRegistry) scanManifests(pluginsDir string) error {
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
		if !m.HasHookPlugin() {
			continue
		}
		r.manifests[m.Name] = m
	}
	return nil
}

// HookPluginEntry is the operator-visible view of a registered plugin's
// hook surface.
type HookPluginEntry struct {
	Name        string           `json:"name"`
	Hooks       []HookPluginHook `json:"hooks"`
	Version     string           `json:"version,omitempty"`
	Description string           `json:"description,omitempty"`
	State       string           `json:"state"` // "pending" | "approved"
}

// HookPluginHook is one hook subscription within a plugin's surface.
type HookPluginHook struct {
	Event    string `json:"event"`
	Endpoint string `json:"endpoint"`
}

// List returns every hook-plugin manifest, pending or approved.
func (r *HookPluginRegistry) List() []HookPluginEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]HookPluginEntry, 0, len(r.manifests))
	for name, m := range r.manifests {
		state := "pending"
		if _, ok := r.tokens[name]; ok {
			state = "approved"
		}
		hooks := make([]HookPluginHook, 0, len(m.Hooks))
		for _, h := range m.Hooks {
			if strings.TrimSpace(h.Event) == "" || strings.TrimSpace(h.Endpoint) == "" {
				continue
			}
			hooks = append(hooks, HookPluginHook(h))
		}
		out = append(out, HookPluginEntry{
			Name:        name,
			Hooks:       hooks,
			Version:     m.Version,
			Description: m.Description,
			State:       state,
		})
	}
	return out
}

// Approve flips a hook plugin to approved. Idempotent.
func (r *HookPluginRegistry) Approve(name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.manifests[name]; !ok {
		return "", fmt.Errorf("plugin %q not found (no manifest with hooks)", name)
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

// Revoke removes a hook plugin's token. Idempotent.
func (r *HookPluginRegistry) Revoke(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tokens[name]; !ok {
		return nil
	}
	delete(r.tokens, name)
	return r.saveTokensLocked()
}

// LookupApprovedManifest returns the manifest for an approved hook
// plugin, or (zero, false) if unknown / pending.
func (r *HookPluginRegistry) LookupApprovedManifest(name string) (Manifest, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.tokens[name]; !ok {
		return Manifest{}, false
	}
	m, ok := r.manifests[name]
	return m, ok
}

// ApprovedManifests returns every approved hook-plugin manifest.
func (r *HookPluginRegistry) ApprovedManifests() []Manifest {
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
// PluginHookDispatcher: hookstore.EventListener that fans events out
// to plugin endpoints declared in approved manifests.
// ─────────────────────────────────────────────────────────────────────

// hookEnvelope is the body the gateway POSTs to each plugin endpoint.
// Matches the design doc's contract: {event, payload, timestamp RFC3339}.
type hookEnvelope struct {
	Event     string         `json:"event"`
	Payload   map[string]any `json:"payload"`
	Timestamp string         `json:"timestamp"`
}

// NewPluginHookDispatcher builds an EventListener that POSTs the
// design-doc hook envelope to every endpoint matching the fired event
// across the supplied approved manifests. The returned listener is
// snapshot-style — it captures the manifest set at construction time.
// Re-approving plugins at runtime requires rebuilding the listener
// (or restarting the gateway; matches the channel/tool-plugin posture).
//
// Failure modes (per design doc):
//   - 5xx / network error → logged to stderr, dropped (at-most-once).
//   - Slow plugin (>10s) → http.Client timeout, logged, dropped.
//   - Missing plugin (connection refused) → logged, dropped.
//
// The listener is fire-and-forget: it returns immediately and POSTs
// each endpoint in its own goroutine so a slow plugin doesn't block
// the others.
func NewPluginHookDispatcher(approved []Manifest) hookstore.EventListener {
	// Build event → []endpoint index so dispatch is O(matching endpoints)
	// rather than O(plugins × hooks).
	byEvent := map[string][]string{}
	for _, m := range approved {
		for _, h := range m.Hooks {
			ev := strings.TrimSpace(h.Event)
			ep := strings.TrimSpace(h.Endpoint)
			if ev == "" || ep == "" {
				continue
			}
			byEvent[ev] = append(byEvent[ev], ep)
		}
	}
	client := &http.Client{Timeout: 10 * time.Second}
	return func(event hookstore.EventType, payload map[string]any) {
		endpoints := byEvent[string(event)]
		if len(endpoints) == 0 {
			return
		}
		envelope := hookEnvelope{
			Event:     string(event),
			Payload:   payload,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}
		raw, err := json.Marshal(envelope)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[hook-plugin:%s] envelope marshal: %v\n", event, err)
			return
		}
		for _, endpoint := range endpoints {
			// Go 1.22+: range-loop variables are per-iteration, so the
			// goroutine captures this iteration's `endpoint` safely.
			go func() {
				req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
				if err != nil {
					fmt.Fprintf(os.Stderr, "[hook-plugin:%s] request build: %v\n", event, err)
					return
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[hook-plugin:%s] POST %s: %v\n", event, endpoint, err)
					return
				}
				resp.Body.Close()
				if resp.StatusCode >= 400 {
					fmt.Fprintf(os.Stderr, "[hook-plugin:%s] POST %s: %d\n", event, endpoint, resp.StatusCode)
				}
			}()
		}
	}
}

package plugins

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
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

	"openclaw-go/internal/channels"
	"openclaw-go/internal/fileutil"
)

// ChannelPluginRegistry tracks approved channel plugins + their tokens.
// Tokens are persisted to a sidecar file (mode 0o600) so they survive
// gateway restarts — plugins don't have to re-handshake after a reboot.
//
// Threading model: the Registry is constructed once at startup, then
// approve/revoke is called from RPC handlers (one at a time per request)
// and the channel-side Send/handler reads token state under RLock.
type ChannelPluginRegistry struct {
	mu         sync.RWMutex
	tokensFile string
	// tokens maps plugin name to its bearer token. Empty/missing entry =
	// not approved.
	tokens map[string]string
	// manifests is the merged view of what's on disk under pluginsDir.
	// Loaded once at startup so List() doesn't re-scan the filesystem
	// on every RPC.
	manifests map[string]Manifest
}

// NewChannelPluginRegistry constructs a registry rooted at the supplied
// plugins directory + tokens persistence path. Tokens file is created
// at mode 0o600 on first save.
func NewChannelPluginRegistry(pluginsDir, tokensFile string) (*ChannelPluginRegistry, error) {
	r := &ChannelPluginRegistry{
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

func (r *ChannelPluginRegistry) loadTokens() error {
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

func (r *ChannelPluginRegistry) saveTokensLocked() error {
	raw, err := json.MarshalIndent(r.tokens, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.tokensFile), 0o755); err != nil {
		return err
	}
	return fileutil.WriteFile(r.tokensFile, raw, 0o600)
}

// scanManifests walks the plugins directory and indexes channel-plugin
// manifests for later approval/listing. Non-channel plugins (route-only
// or tool-only) are NOT tracked here — they go through the existing
// Loader.
func (r *ChannelPluginRegistry) scanManifests(pluginsDir string) error {
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
		if !m.HasChannelPlugin() {
			continue
		}
		r.manifests[m.Name] = m
	}
	return nil
}

// ChannelPluginEntry is the operator-visible view of a registered plugin.
// State is "approved" when a token has been generated, "pending" otherwise.
type ChannelPluginEntry struct {
	Name        string `json:"name"`
	Channel     string `json:"channel"`
	BaseURL     string `json:"baseUrl"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	State       string `json:"state"` // "pending" | "approved"
}

// List returns every channel-plugin manifest the registry knows about,
// pending or approved, sorted by name.
func (r *ChannelPluginRegistry) List() []ChannelPluginEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ChannelPluginEntry, 0, len(r.manifests))
	for name, m := range r.manifests {
		state := "pending"
		if _, ok := r.tokens[name]; ok {
			state = "approved"
		}
		out = append(out, ChannelPluginEntry{
			Name:        name,
			Channel:     m.Channel.Channel,
			BaseURL:     m.Channel.BaseURL,
			Version:     m.Version,
			Description: m.Description,
			State:       state,
		})
	}
	return out
}

// Approve flips a plugin from pending → approved, generating a fresh
// bearer token. Returns the token so the operator can copy it into the
// plugin's environment (`OPENCLAW_PLUGIN_TOKEN`). Idempotent — if already
// approved, returns the existing token rather than rotating it
// silently (rotation needs an explicit Revoke + Approve).
func (r *ChannelPluginRegistry) Approve(name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.manifests[name]; !ok {
		return "", fmt.Errorf("plugin %q not found (no manifest)", name)
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

// Revoke removes a plugin's token, putting it back in "pending" state.
// Idempotent — revoking an unknown name is a no-op.
func (r *ChannelPluginRegistry) Revoke(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tokens[name]; !ok {
		return nil
	}
	delete(r.tokens, name)
	return r.saveTokensLocked()
}

// LookupApprovedManifest returns the manifest for an approved plugin, or
// (zero, false) if the plugin is unknown OR pending. Used by the gateway
// when building pluginChannel instances at startup.
func (r *ChannelPluginRegistry) LookupApprovedManifest(name string) (Manifest, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.tokens[name]; !ok {
		return Manifest{}, false
	}
	m, ok := r.manifests[name]
	return m, ok
}

// ApprovedManifests returns every approved channel-plugin manifest. Used
// during gateway init to construct + register pluginChannels.
func (r *ChannelPluginRegistry) ApprovedManifests() []Manifest {
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

// tokenForPlugin returns the registered bearer token for name, or "" if
// the plugin is pending/unknown. Constant-time comparison happens at the
// callsite (handler) via hmac.Equal.
func (r *ChannelPluginRegistry) tokenForPlugin(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tokens[name]
}

// generatePluginToken returns a 32-byte hex-encoded random string (256
// bits of entropy). Used as the per-plugin bearer token; same format as
// the web-login nonce so operators see a consistent token shape.
func generatePluginToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ─────────────────────────────────────────────────────────────────────
// pluginChannel: the gateway-side implementation of channels.Channel
// that forwards outbound messages to the plugin's HTTP server.
// ─────────────────────────────────────────────────────────────────────

// pluginChannel adapts a ChannelManifest into a channels.Channel. The
// gateway's router treats it identically to a built-in channel — Send is
// invoked, an HTTP roundtrip happens, errors propagate.
type pluginChannel struct {
	name    string // channel name (Manifest.Channel.Channel)
	sendURL string // plugin's POST endpoint for outbound delivery
	client  *http.Client
}

// NewPluginChannel constructs a channels.Channel that delegates to the
// plugin's send endpoint. Empty BaseURL produces a disabled channel that
// returns nil from Send — matches the pattern of misconfigured built-ins.
func NewPluginChannel(m Manifest) channels.Channel {
	base := strings.TrimRight(strings.TrimSpace(m.Channel.BaseURL), "/")
	return &pluginChannel{
		name:    m.Channel.Channel,
		sendURL: base + "/channel/send",
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *pluginChannel) Name() string { return p.name }

func (p *pluginChannel) Send(ctx context.Context, message channels.OutboundMessage) error {
	if p.sendURL == "/channel/send" {
		// BaseURL was empty — channel is disabled.
		return nil
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.sendURL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("plugin %s: POST %s: %w", p.name, p.sendURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("plugin %s: %d: %s", p.name, resp.StatusCode, string(body))
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────
// Inbound handler: the gateway endpoint plugins POST to with new messages
// from their respective platforms.
// ─────────────────────────────────────────────────────────────────────

// BuildChannelPluginInboundHandler returns an http.HandlerFunc to mount at
// `/plugins/{name}/inbound`. Plugins POST a channels.InboundMessage JSON
// body with an `Authorization: Bearer <token>` header. The handler:
//
//   - looks up the registered token for the plugin name in the URL,
//   - verifies the Bearer header in constant time,
//   - forwards the InboundMessage to the supplied dispatch callback
//     (typically server.HandleInbound).
//
// Unknown plugin → 404. Bad/missing token → 401. Decode failure → 400.
// Dispatch error → 500. Success → 200 with `{"ok":true}` body.
func BuildChannelPluginInboundHandler(
	reg *ChannelPluginRegistry,
	dispatch func(context.Context, channels.InboundMessage) error,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// URL shape: /plugins/{name}/inbound
		path := strings.TrimPrefix(r.URL.Path, "/plugins/")
		path = strings.TrimSuffix(path, "/inbound")
		name := strings.TrimSuffix(path, "/")
		if name == "" || strings.Contains(name, "/") {
			http.NotFound(w, r)
			return
		}
		want := reg.tokenForPlugin(name)
		if want == "" {
			http.NotFound(w, r) // pending or unknown
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		got = strings.TrimSpace(got)
		if got == "" || !hmac.Equal([]byte(got), []byte(want)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var msg channels.InboundMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		// Authority: the gateway sets `Channel` from the manifest, not
		// from the plugin-supplied body. Prevents a misbehaving plugin
		// from masquerading as a different channel.
		if m, ok := reg.LookupApprovedManifest(name); ok {
			msg.Channel = m.Channel.Channel
		}
		if err := dispatch(r.Context(), msg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

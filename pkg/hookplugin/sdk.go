// Package hookplugin is the SDK plugin authors import when building a
// hook plugin for openclaw-go. It hides the on-the-wire contract for
// receiving event notifications from the gateway behind a small struct
// API:
//
//	func main() {
//	    p, err := hookplugin.LoadFromEnv()
//	    if err != nil { log.Fatal(err) }
//	    p.OnEvent("agent.run.complete", func(ctx context.Context, env hookplugin.Envelope) {
//	        log.Printf("agent run %s finished: %v", env.Payload["sessionId"], env.Payload)
//	    })
//	    log.Fatal(p.Listen(":9301"))
//	}
//
// The plugin's manifest declares one entry per event subscription:
//
//	{
//	  "name": "audit-plugin",
//	  "hooks": [
//	    {"event": "agent.run.complete", "endpoint": "http://127.0.0.1:9301/hook/agent"},
//	    {"event": "approval.requested", "endpoint": "http://127.0.0.1:9301/hook/approval"}
//	  ]
//	}
//
// The same plugin process can subscribe to multiple events at distinct
// paths under /hook/{label}. Path matching is by full URL, so endpoint
// paths are arbitrary — register handlers by the path you put in the
// manifest.
//
// Operator-side config travels via three env vars set when launching
// the plugin process (the gateway prints them after approve):
//
//	OPENCLAW_PLUGIN_NAME    — must match manifest.name
//	OPENCLAW_GATEWAY_URL    — e.g. http://127.0.0.1:18789
//	OPENCLAW_PLUGIN_TOKEN   — issued via `openclaw plugins hook approve <name>`
//
// Hooks are fire-and-forget per docs/PLUGIN-ARCHITECTURE.md: the gateway
// does NOT retry on 5xx, does NOT wait for a response body. Your handler
// should return promptly (any work it kicks off needs its own goroutine
// or queue).
package hookplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Envelope is the body shape the gateway POSTs to each registered hook
// endpoint. Matches docs/PLUGIN-ARCHITECTURE.md section 3.
type Envelope struct {
	Event     string         `json:"event"`
	Payload   map[string]any `json:"payload"`
	Timestamp string         `json:"timestamp"`
}

// Handler is the signature plugin authors implement for each event
// subscription. ctx is the request context (cancellation propagates;
// handlers should respect it for any blocking work). env carries the
// event name + payload + timestamp.
//
// Handlers return no error: hooks are fire-and-forget. To signal
// failure, the handler returns a non-2xx status from its own logic —
// the SDK responds 2xx on every successful handler invocation. If a
// handler panics, the SDK recovers and writes a 500.
type Handler func(ctx context.Context, env Envelope)

// Plugin is the operator-facing handle. Construct via LoadFromEnv (or
// fill the struct yourself for tests). Register one Handler per
// endpoint path the manifest declares; the SDK dispatches incoming
// POSTs to the matching handler.
type Plugin struct {
	Name       string
	GatewayURL string // reserved for future gateway-direction calls
	Token      string // reserved for future verification

	mu       sync.RWMutex
	handlers map[string]Handler // key: URL path (e.g. "/hook/agent")
}

// LoadFromEnv reads the three required env vars and returns a Plugin
// with no handlers registered.
func LoadFromEnv() (*Plugin, error) {
	name := strings.TrimSpace(os.Getenv("OPENCLAW_PLUGIN_NAME"))
	gw := strings.TrimSpace(os.Getenv("OPENCLAW_GATEWAY_URL"))
	tok := strings.TrimSpace(os.Getenv("OPENCLAW_PLUGIN_TOKEN"))
	missing := []string{}
	if name == "" {
		missing = append(missing, "OPENCLAW_PLUGIN_NAME")
	}
	if gw == "" {
		missing = append(missing, "OPENCLAW_GATEWAY_URL")
	}
	if tok == "" {
		missing = append(missing, "OPENCLAW_PLUGIN_TOKEN")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return &Plugin{
		Name:       name,
		GatewayURL: gw,
		Token:      tok,
		handlers:   map[string]Handler{},
	}, nil
}

// HandlePath registers a Handler at the exact URL path the manifest
// declares. The path string should match what's in manifest.hooks[].
// endpoint after the host:port — e.g. for endpoint
// "http://127.0.0.1:9301/hook/agent", call HandlePath("/hook/agent", ...).
//
// Registering the same path twice overwrites the previous handler.
// Empty paths or nil handlers are no-ops.
func (p *Plugin) HandlePath(path string, h Handler) {
	if h == nil {
		return
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.handlers == nil {
		p.handlers = map[string]Handler{}
	}
	p.handlers[path] = h
}

// Paths returns the registered handler paths, sorted alphabetically.
func (p *Plugin) Paths() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.handlers))
	for path := range p.handlers {
		out = append(out, path)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Handler returns the http.Handler the plugin serves. Each registered
// path matches by exact URL.Path; unregistered paths return 404.
func (p *Plugin) Handler() http.Handler {
	return http.HandlerFunc(p.dispatch)
}

// Listen starts the plugin's HTTP server at addr. Blocks until the
// listener exits.
func (p *Plugin) Listen(addr string) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           p.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return server.ListenAndServe()
}

func (p *Plugin) dispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p.mu.RLock()
	h, ok := p.handlers[r.URL.Path]
	p.mu.RUnlock()
	if !ok || h == nil {
		http.NotFound(w, r)
		return
	}
	var env Envelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		http.Error(w, "invalid envelope: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Recover from handler panics — fire-and-forget hooks should not
	// take the plugin process down because of a single bad event.
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[hookplugin] handler %s panicked: %v\n", r.URL.Path, rec)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}()
	h(r.Context(), env)
	// Body is intentionally empty — gateway ignores it per the
	// fire-and-forget contract.
	w.WriteHeader(http.StatusOK)
}

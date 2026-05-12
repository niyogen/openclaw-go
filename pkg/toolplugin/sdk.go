// Package toolplugin is the SDK plugin authors import when building a
// tool plugin for openclaw-go. It hides the on-the-wire contract behind
// a small struct API:
//
//	func main() {
//	    p, err := toolplugin.LoadFromEnv()
//	    if err != nil { log.Fatal(err) }
//	    p.RegisterTool("weather", func(ctx context.Context, args map[string]any) (any, error) {
//	        city, _ := args["city"].(string)
//	        return fmt.Sprintf("sunny in %s", city), nil
//	    })
//	    log.Fatal(p.Listen(":9201"))
//	}
//
// The plugin's manifest declares one entry per tool with a path under
// the plugin's base URL:
//
//	{
//	  "name": "weather-plugin",
//	  "tools": [
//	    {"name": "weather", "endpoint": "http://127.0.0.1:9201/tool/weather"}
//	  ]
//	}
//
// Operator-side config travels via three env vars set when launching the
// plugin process (the gateway prints them after approve):
//
//	OPENCLAW_PLUGIN_NAME    — must match manifest.name
//	OPENCLAW_GATEWAY_URL    — e.g. http://127.0.0.1:18789
//	OPENCLAW_PLUGIN_TOKEN   — issued via `openclaw plugins tool approve <name>`
//
// The token is reserved for future plugin → gateway callbacks (streaming
// tool results, async completions). The gateway does NOT currently send
// it when invoking a tool — plugins should bind loopback for security
// if they don't have other auth.
//
// See docs/PLUGIN-ARCHITECTURE.md for the full contract.
package toolplugin

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

// Handler is the signature plugin authors implement for each tool. The
// gateway invokes it via HTTP; ctx is the request context (cancellation
// propagates), args is the JSON-decoded `arguments` map from
// `tools.invoke`. Return either a JSON-marshallable result OR an error
// — the SDK encodes both into the response envelope automatically.
type Handler func(ctx context.Context, args map[string]any) (any, error)

// Plugin is the operator-facing handle. Construct via LoadFromEnv (or
// fill the struct yourself for tests). Register one handler per tool
// the plugin exposes, then call Listen to start serving.
type Plugin struct {
	// Name MUST match the `name` field in your plugin.json manifest.
	Name string
	// GatewayURL is where the gateway listens (typically
	// http://127.0.0.1:18789 in local deployments). Reserved for
	// future callbacks; not used by the current request/response
	// tool contract.
	GatewayURL string
	// Token is the bearer token issued by `openclaw plugins tool
	// approve`. Reserved for future callbacks; not currently used.
	Token string

	mu    sync.RWMutex
	tools map[string]Handler
}

// LoadFromEnv reads the three required env vars and returns a configured
// Plugin (with no tools registered yet). Returns a clear error when any
// var is missing — fail loudly at startup rather than silently dropping
// every invocation.
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
		tools:      map[string]Handler{},
	}, nil
}

// RegisterTool wires a Handler to a tool name. Tools are served at
// `/tool/{name}` on the plugin's HTTP server — so this:
//
//	p.RegisterTool("weather", weatherHandler)
//
// corresponds to a manifest entry with endpoint
// `http://127.0.0.1:{port}/tool/weather`. Registering the same name
// twice overwrites the previous handler (last-write-wins).
func (p *Plugin) RegisterTool(name string, h Handler) {
	if h == nil {
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tools == nil {
		p.tools = map[string]Handler{}
	}
	p.tools[name] = h
}

// Tools returns the registered tool names, sorted alphabetically.
// Useful for debug pages and tests.
func (p *Plugin) Tools() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.tools))
	for name := range p.tools {
		out = append(out, name)
	}
	// stable order helps tests and logs; sort.Strings would be O(n log n)
	// but n is small (<10 in practice) so a simple insertion sort works.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Handler returns the http.Handler the plugin would serve. Useful for
// composing with health/metrics routes. Exposes /tool/{name} for each
// registered tool.
func (p *Plugin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/tool/", p.handleInvoke)
	return mux
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

func (p *Plugin) handleInvoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// URL shape: /tool/{name}
	name := strings.TrimPrefix(r.URL.Path, "/tool/")
	name = strings.TrimSpace(strings.Trim(name, "/"))
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	p.mu.RLock()
	h, ok := p.tools[name]
	p.mu.RUnlock()
	if !ok || h == nil {
		http.Error(w, "tool not found: "+name, http.StatusNotFound)
		return
	}
	var args map[string]any
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		// Empty body is allowed — pass nil args through.
		args = nil
	}
	result, err := h(r.Context(), args)
	w.Header().Set("Content-Type", "application/json")
	envelope := struct {
		Result any    `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}{}
	status := http.StatusOK
	if err != nil {
		envelope.Error = err.Error()
		status = http.StatusInternalServerError
	} else {
		envelope.Result = result
	}
	raw, _ := json.Marshal(envelope)
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

package plugins

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Manifest describes an external plugin loaded from a directory.
// Each plugin directory must contain a plugin.json file.
type Manifest struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Description string            `json:"description"`
	Routes      []ManifestRoute   `json:"routes"`
	Tools       []ManifestTool    `json:"tools"`
	Env         map[string]string `json:"env"` // key → env var name for config
}

// ManifestRoute declares an HTTP route the plugin wants to expose via the gateway.
type ManifestRoute struct {
	Method  string `json:"method"`  // GET, POST, etc. (empty = any)
	Path    string `json:"path"`    // e.g. /plugins/myplugin/action
	Forward string `json:"forward"` // URL to reverse-proxy to (optional)
}

// ManifestTool declares a callable tool the plugin exposes.
type ManifestTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Endpoint    string `json:"endpoint"` // POST URL the gateway forwards tool calls to
}

// ExternalPlugin adapts a loaded Manifest to the Plugin interface.
type ExternalPlugin struct {
	manifest Manifest
	dir      string
	client   *http.Client
}

func (p *ExternalPlugin) Name() string { return p.manifest.Name }

func (p *ExternalPlugin) RegisterRoutes(mux *http.ServeMux) {
	for _, route := range p.manifest.Routes {
		route := route // capture
		pattern := route.Path
		if strings.TrimSpace(route.Forward) != "" {
			// Reverse-proxy: forward request to the plugin's process.
			fwd := strings.TrimSuffix(route.Forward, "/")
			client := p.client
			mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
				targetURL := fwd + r.URL.Path
				proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
				if err != nil {
					http.Error(w, "proxy error: "+err.Error(), http.StatusBadGateway)
					return
				}
				for k, vs := range r.Header {
					for _, v := range vs {
						proxyReq.Header.Add(k, v)
					}
				}
				resp, err := client.Do(proxyReq)
				if err != nil {
					http.Error(w, "proxy error: "+err.Error(), http.StatusBadGateway)
					return
				}
				defer resp.Body.Close()
				w.WriteHeader(resp.StatusCode)
				buf := make([]byte, 32*1024)
				for {
					n, _ := resp.Body.Read(buf)
					if n == 0 {
						break
					}
					_, _ = w.Write(buf[:n])
				}
			})
		} else {
			// Static 501 placeholder — plugin declared the route but has no forwarder.
			mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, fmt.Sprintf("plugin %s: no forwarder configured for %s", p.manifest.Name, route.Path), http.StatusNotImplemented)
			})
		}
	}
}

// Tools returns the tools declared by this plugin.
func (p *ExternalPlugin) Tools() []ManifestTool {
	out := make([]ManifestTool, len(p.manifest.Tools))
	copy(out, p.manifest.Tools)
	return out
}

// Loader discovers and loads plugin manifests from a directory.
type Loader struct {
	dir string
}

// NewLoader creates a loader that looks for plugins under dir.
// Each subdirectory that contains a plugin.json is treated as a plugin.
func NewLoader(dir string) *Loader {
	return &Loader{dir: dir}
}

// Load scans the plugin directory and returns one ExternalPlugin per manifest found.
func (l *Loader) Load() ([]*ExternalPlugin, error) {
	if l.dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no plugin dir is not an error
		}
		return nil, err
	}

	var loaded []*ExternalPlugin
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifestPath := filepath.Join(l.dir, entry.Name(), "plugin.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue // no manifest — skip silently
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			continue // invalid manifest — skip
		}
		if strings.TrimSpace(m.Name) == "" {
			m.Name = entry.Name()
		}
		loaded = append(loaded, &ExternalPlugin{
			manifest: m,
			dir:      filepath.Join(l.dir, entry.Name()),
			client:   &http.Client{},
		})
	}
	return loaded, nil
}

package gateway

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// UIDir is the runtime override for the UI directory.
// If non-empty, static files are served from this path.
var UIDir string

//go:embed ui/*
var embeddedUI embed.FS

// registerUIRoutes mounts the control UI under /ui/.
// If UIDir is set, files are served from that directory (for development).
// Otherwise the embedded files are used.
//
// The UI directory ships empty by default; copy a built SPA there to enable.
func (s *Server) registerUIRoutes() {
	var fileServer http.Handler

	if UIDir != "" {
		// Development mode: serve from filesystem.
		fileServer = http.StripPrefix("/ui", http.FileServer(http.Dir(UIDir)))
	} else {
		// Production: serve embedded assets.
		sub, err := fs.Sub(embeddedUI, "ui")
		if err != nil {
			return // no UI assets embedded — skip
		}
		fileServer = http.StripPrefix("/ui", http.FileServer(http.FS(sub)))
	}

	// Serve /ui and /ui/* — auth-guarded, SPA fallback for client-side routing.
	s.mux.Handle("/ui/", s.withAuth(func(w http.ResponseWriter, r *http.Request) {
		// Try the exact path first; fall back to index.html for SPA routing.
		if UIDir != "" {
			root := filepath.Clean(UIDir)
			reqPath := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(r.URL.Path, "/ui")))
			// Guard against path traversal: resolved path must stay inside UIDir.
			if !strings.HasPrefix(reqPath, root+string(filepath.Separator)) && reqPath != root {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if _, err := os.Stat(reqPath); os.IsNotExist(err) && !strings.Contains(r.URL.Path, ".") {
				http.ServeFile(w, r, filepath.Join(root, "index.html"))
				return
			}
		}
		fileServer.ServeHTTP(w, r)
	}))

	// Redirect / → /ui for convenience (no auth required on redirect itself).
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/ui/", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})
}

package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeManifest(t *testing.T, dir, name string, m Manifest) {
	t.Helper()
	pluginDir := filepath.Join(dir, name)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoaderEmpty(t *testing.T) {
	l := NewLoader(t.TempDir())
	plugins, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 0 {
		t.Fatalf("expected 0 plugins, got %d", len(plugins))
	}
}

func TestLoaderNonExistentDir(t *testing.T) {
	l := NewLoader("/no/such/dir/openclaw-go-plugins")
	plugins, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 0 {
		t.Fatalf("expected 0 plugins, got %d", len(plugins))
	}
}

func TestLoaderLoadsManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "my-plugin", Manifest{
		Name:        "my-plugin",
		Version:     "1.0.0",
		Description: "test plugin",
		Routes: []ManifestRoute{
			{Method: "GET", Path: "/plugins/my-plugin/ping", Forward: "http://localhost:9999"},
		},
		Tools: []ManifestTool{
			{Name: "my-plugin.hello", Description: "say hello", Endpoint: "http://localhost:9999/tools/hello"},
		},
	})

	l := NewLoader(dir)
	loaded, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(loaded))
	}
	p := loaded[0]
	if p.Name() != "my-plugin" {
		t.Fatalf("unexpected name: %s", p.Name())
	}
	if len(p.Tools()) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(p.Tools()))
	}
}

func TestLoaderSkipsInvalid(t *testing.T) {
	dir := t.TempDir()
	// Valid plugin.
	writeManifest(t, dir, "good", Manifest{Name: "good"})
	// Directory with no manifest — should be silently skipped.
	if err := os.MkdirAll(filepath.Join(dir, "no-manifest"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Directory with invalid JSON — should be silently skipped.
	badDir := filepath.Join(dir, "bad-json")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "plugin.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	l := NewLoader(dir)
	loaded, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 plugin (only 'good'), got %d", len(loaded))
	}
}

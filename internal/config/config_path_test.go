package config

import (
	"path/filepath"
	"testing"
)

func TestDefaultPathUsesOpenclawConfigPathEnv(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "nested", "openclaw.json")
	t.Setenv("OPENCLAW_CONFIG_PATH", want)
	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

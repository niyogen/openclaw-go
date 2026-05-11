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

func TestLoadMissingFileAppliesGatewayHostEnv(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "missing-dir", "openclaw.json")
	t.Setenv("OPENCLAW_CONFIG_PATH", cfgPath)
	t.Setenv("OPENCLAW_GATEWAY_HOST", "0.0.0.0")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.Host != "0.0.0.0" {
		t.Fatalf("gateway host: got %q want 0.0.0.0", cfg.Gateway.Host)
	}
}

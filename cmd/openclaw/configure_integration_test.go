//go:build integration

package main

import (
	"path/filepath"
	"testing"

	"openclaw-go/internal/config"
)

// Exercises real config I/O + configure gateway path (same as CLI), using a temp
// file via OPENCLAW_CONFIG_PATH so the operator home directory is untouched.
func TestIntegrationConfigureGatewayMetricsRequireAuthRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	t.Setenv("OPENCLAW_CONFIG_PATH", path)

	cfg := config.Default()
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Gateway.MetricsRequireAuth {
		t.Fatal("expected default false")
	}

	if err := runConfigureGateway(loaded, []string{"metrics-require-auth", "true"}); err != nil {
		t.Fatal(err)
	}
	afterTrue, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !afterTrue.Gateway.MetricsRequireAuth {
		t.Fatal("expected metricsRequireAuth true after configure")
	}

	if err := runConfigureGateway(afterTrue, []string{"metrics-require-auth", "false"}); err != nil {
		t.Fatal(err)
	}
	afterFalse, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if afterFalse.Gateway.MetricsRequireAuth {
		t.Fatal("expected metricsRequireAuth false after configure")
	}
}

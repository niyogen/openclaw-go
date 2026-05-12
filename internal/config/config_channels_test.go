package config

import (
	"path/filepath"
	"testing"
)

// TestDefaultIncludesNewChannels pins the default-shape for the channels
// added in 2026-05-12 (email, signal, matrix, mattermost). A regression
// here means operators upgrading would see their channel section silently
// lose entries.
func TestDefaultIncludesNewChannels(t *testing.T) {
	cfg := Default()
	if cfg.Channels.Email.Enabled {
		t.Error("Email should default to disabled")
	}
	if cfg.Channels.Email.Port != 587 {
		t.Errorf("Email.Port default: got %d want 587", cfg.Channels.Email.Port)
	}
	if cfg.Channels.Signal.Enabled {
		t.Error("Signal should default to disabled")
	}
	if cfg.Channels.Matrix.Enabled {
		t.Error("Matrix should default to disabled")
	}
	if cfg.Channels.Mattermost.Enabled {
		t.Error("Mattermost should default to disabled")
	}
}

// TestSaveLoadPreservesNewChannelConfig round-trips a non-default config
// through Save/Load. If a new struct field is added but missed in the
// JSON shape (forgot a tag, wrong field name) this catches it.
func TestSaveLoadPreservesNewChannelConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openclaw.json")

	cfg := Default()
	cfg.Channels.Email = EmailChannelConfig{
		Enabled: true, Host: "smtp.example.com", Port: 465,
		Username: "bot@example.com", Password: "app-pw", From: "bot@example.com",
	}
	cfg.Channels.Signal = SignalChannelConfig{
		Enabled: true, BaseURL: "http://127.0.0.1:8080", Number: "+15550001111",
	}
	cfg.Channels.Matrix = MatrixChannelConfig{
		Enabled: true, BaseURL: "https://matrix.example.com", AccessToken: "syt_xyz",
	}
	cfg.Channels.Mattermost = MattermostChannelConfig{
		Enabled: true, BaseURL: "https://mm.example.com", AccessToken: "mm-tok",
	}

	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Channels.Email != cfg.Channels.Email {
		t.Errorf("email roundtrip: got %+v", got.Channels.Email)
	}
	if got.Channels.Signal != cfg.Channels.Signal {
		t.Errorf("signal roundtrip: got %+v", got.Channels.Signal)
	}
	if got.Channels.Matrix != cfg.Channels.Matrix {
		t.Errorf("matrix roundtrip: got %+v", got.Channels.Matrix)
	}
	if got.Channels.Mattermost != cfg.Channels.Mattermost {
		t.Errorf("mattermost roundtrip: got %+v", got.Channels.Mattermost)
	}
}

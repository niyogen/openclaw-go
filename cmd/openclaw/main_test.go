package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openclaw-go/internal/config"
)

func TestParseOnboardFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    onboardOptions
		wantErr bool
	}{
		{
			name: "no flags",
			args: nil,
			want: onboardOptions{},
		},
		{
			name: "all flags",
			args: []string{
				"--provider", "openai",
				"--openai-key", "sk-123",
				"--anthropic-key", "ant-xyz",
				"--gateway-token", "tok",
				"--gateway-port", "9090",
			},
			want: onboardOptions{
				provider: "openai", openaiKey: "sk-123",
				anthropicKey: "ant-xyz", gatewayToken: "tok",
				gatewayPort: "9090", anyFlagGiven: true,
			},
		},
		{
			name: "claude alias normalises to anthropic",
			args: []string{"--provider", "claude"},
			want: onboardOptions{provider: "anthropic", anyFlagGiven: true},
		},
		{
			name:    "invalid provider",
			args:    []string{"--provider", "groq"},
			wantErr: true,
		},
		{
			name:    "unknown flag",
			args:    []string{"--magic", "yes"},
			wantErr: true,
		},
		{
			name:    "trailing flag without value",
			args:    []string{"--provider"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOnboardFlags(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestApplyOnboardOptions(t *testing.T) {
	cfg := config.Default()
	applyOnboardOptions(&cfg, onboardOptions{
		provider:     "openai",
		openaiKey:    "sk-real",
		anthropicKey: "ant-real",
		gatewayToken: "secret",
		gatewayPort:  "9123",
		anyFlagGiven: true,
	})
	if cfg.Agent.Provider != "openai" {
		t.Fatalf("provider: %q", cfg.Agent.Provider)
	}
	if cfg.Providers.OpenAI.APIKey != "sk-real" {
		t.Fatalf("openai key: %q", cfg.Providers.OpenAI.APIKey)
	}
	if cfg.Providers.Anthropic.APIKey != "ant-real" {
		t.Fatalf("anthropic key: %q", cfg.Providers.Anthropic.APIKey)
	}
	if cfg.Gateway.AuthToken != "secret" {
		t.Fatalf("auth token: %q", cfg.Gateway.AuthToken)
	}
	if cfg.Gateway.Port != 9123 {
		t.Fatalf("port: %d", cfg.Gateway.Port)
	}
}

func TestApplyOnboardOptionsIgnoresInvalidPort(t *testing.T) {
	cfg := config.Default()
	originalPort := cfg.Gateway.Port
	applyOnboardOptions(&cfg, onboardOptions{gatewayPort: "not-a-port", anyFlagGiven: true})
	if cfg.Gateway.Port != originalPort {
		t.Fatalf("invalid port should be ignored: got %d", cfg.Gateway.Port)
	}
}

func TestRunRestoreRequiresYes(t *testing.T) {
	if err := runRestore([]string{"/nonexistent/backup"}); err == nil {
		t.Fatal("expected error without --yes")
	}
}

func TestRunRestoreRequiresArgs(t *testing.T) {
	if err := runRestore(nil); err == nil {
		t.Fatal("expected usage error with no args")
	}
}

func TestRunRestoreCopiesBackupOverDataDir(t *testing.T) {
	home := t.TempDir()
	// Both Windows (USERPROFILE) and Unix (HOME) lookups consult these env
	// vars via os.UserHomeDir; setting both is the cross-platform belt-and-
	// braces approach. t.Setenv restores prior values on teardown.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	backup := filepath.Join(home, "mybackup")
	if err := os.MkdirAll(filepath.Join(backup, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "marker.txt"), []byte("from-backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "subdir", "nested.txt"), []byte("nested-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runRestore([]string{backup, "--yes"}); err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	dataDir := filepath.Join(home, ".openclaw-go")
	gotMarker, err := os.ReadFile(filepath.Join(dataDir, "marker.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotMarker) != "from-backup" {
		t.Fatalf("marker content: %q", gotMarker)
	}
	gotNested, err := os.ReadFile(filepath.Join(dataDir, "subdir", "nested.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotNested) != "nested-content" {
		t.Fatalf("nested content: %q", gotNested)
	}
}

func TestRunRestoreRejectsNonDirBackup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	notDir := filepath.Join(home, "regularfile.txt")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runRestore([]string{notDir, "--yes"}); err == nil {
		t.Fatal("expected error for non-directory backup path")
	}
}

func TestDashboardURLDefaults(t *testing.T) {
	cfg := config.Config{}
	got := dashboardURL(cfg)
	if got != "http://127.0.0.1:8080/ui/" {
		t.Fatalf("default URL: %s", got)
	}
}

func TestDashboardURLRespectsConfig(t *testing.T) {
	cfg := config.Config{}
	cfg.Gateway.Host = "0.0.0.0"
	cfg.Gateway.Port = 9123
	got := dashboardURL(cfg)
	if got != "http://0.0.0.0:9123/ui/" {
		t.Fatalf("configured URL: %s", got)
	}
}

func TestRunDashboardSurvivesBrowserOpenFailure(t *testing.T) {
	// Swap the launcher with one that always errors. runDashboard must
	// still return nil — opening the browser is best-effort.
	orig := execOpen
	t.Cleanup(func() { execOpen = orig })
	execOpen = func(name string, args ...string) error {
		return errors.New("no launcher available")
	}
	cfg := config.Config{}
	cfg.Gateway.Host = "127.0.0.1"
	cfg.Gateway.Port = 8080
	if err := runDashboard(cfg); err != nil {
		t.Fatalf("runDashboard should not error on browser open failure: %v", err)
	}
}

func TestOpenBrowserDispatchesByGOOS(t *testing.T) {
	origGoos := goos
	origExec := execOpen
	t.Cleanup(func() {
		goos = origGoos
		execOpen = origExec
	})

	cases := map[string]string{
		"windows": "rundll32",
		"darwin":  "open",
		"linux":   "xdg-open",
		"freebsd": "xdg-open", // default branch
	}
	for osName, wantCmd := range cases {
		t.Run(osName, func(t *testing.T) {
			goos = func() string { return osName }
			var captured string
			execOpen = func(name string, args ...string) error {
				captured = name
				return nil
			}
			_ = openBrowser("http://example.com")
			if captured != wantCmd {
				t.Fatalf("got launcher %q want %q", captured, wantCmd)
			}
		})
	}
}

func TestRunMessageUsageErrors(t *testing.T) {
	cases := [][]string{
		nil,
		{"send"},
		{"send", "sess"},
		{"history"},
		{"dispatch"},
		{"dispatch", "telegram"},
		{"dispatch", "telegram", "+123"},
		{"unknown"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			if err := runMessage("http://127.0.0.1:1", args); err == nil {
				t.Fatalf("expected usage error for args %v", args)
			}
		})
	}
}

func TestRunMessageSendPostsToMessageEndpoint(t *testing.T) {
	var seenPath, seenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	if err := runMessage(srv.URL, []string{"send", "sess-1", "hello world"}); err != nil {
		t.Fatal(err)
	}
	if seenPath != "/message" {
		t.Fatalf("path: %s", seenPath)
	}
	if !strings.Contains(seenBody, `"sessionId":"sess-1"`) || !strings.Contains(seenBody, `"message":"hello world"`) {
		t.Fatalf("body: %s", seenBody)
	}
}

func TestRunMessageHistoryHitsHistoryEndpoint(t *testing.T) {
	var seenPath, seenMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenMethod = r.Method
		_, _ = w.Write([]byte(`{"messages":[]}`))
	}))
	t.Cleanup(srv.Close)
	if err := runMessage(srv.URL, []string{"history", "sess-2"}); err != nil {
		t.Fatal(err)
	}
	if seenMethod != http.MethodGet {
		t.Fatalf("method: %s", seenMethod)
	}
	if seenPath != "/sessions/sess-2/history" {
		t.Fatalf("path: %s", seenPath)
	}
}

func TestRunMessageDispatchUsesRPC(t *testing.T) {
	var seenMethod string
	var seenParams map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string
			Params map[string]any
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		seenMethod = req.Method
		seenParams = req.Params
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	t.Cleanup(srv.Close)
	if err := runMessage(srv.URL, []string{"dispatch", "telegram", "+15551112222", "hi"}); err != nil {
		t.Fatal(err)
	}
	if seenMethod != "message.send" {
		t.Fatalf("method: %s", seenMethod)
	}
	if seenParams["channel"] != "telegram" || seenParams["target"] != "+15551112222" || seenParams["message"] != "hi" {
		t.Fatalf("params: %+v", seenParams)
	}
}

func TestRunCompactionUsageErrors(t *testing.T) {
	cases := [][]string{
		nil,
		{"list"},              // missing session id
		{"get"},               // missing id
		{"restore"},           // missing id
		{"restore", "the-id"}, // missing --yes
		{"branch"},            // missing id
		{"unknown-subcmd"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			if err := runCompactionCLI("http://127.0.0.1:1", args); err == nil {
				t.Fatalf("expected usage error for args %v", args)
			}
		})
	}
}

func TestRunCompactionList(t *testing.T) {
	var seenMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct{ Method string }
		_ = json.Unmarshal(body, &req)
		seenMethod = req.Method
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[]}`))
	}))
	t.Cleanup(srv.Close)
	if err := runCompactionCLI(srv.URL, []string{"list", "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if seenMethod != "sessions.compaction.list" {
		t.Fatalf("method: %s", seenMethod)
	}
}

func TestRunCompactionBranchPassesNewID(t *testing.T) {
	var seenParams map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct{ Params map[string]any }
		_ = json.Unmarshal(body, &req)
		seenParams = req.Params
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(srv.Close)
	if err := runCompactionCLI(srv.URL, []string{"branch", "cmp-1", "--id", "fork-A"}); err != nil {
		t.Fatal(err)
	}
	if seenParams["id"] != "cmp-1" || seenParams["newSessionId"] != "fork-A" {
		t.Fatalf("params: %+v", seenParams)
	}
}

func TestRunWebLoginApprovedFlow(t *testing.T) {
	// Mock RPC server returning a start response then an approved wait response.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"nonce":"abc","url":"/web/login/abc"}}`))
		case 2:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"status":"approved","issuedToken":"new-tok"}}`))
		}
	}))
	t.Cleanup(srv.Close)

	// Suppress browser launch — point execOpen at a no-op so the test doesn't
	// spawn rundll32 on Windows runners.
	origExec := execOpen
	t.Cleanup(func() { execOpen = origExec })
	execOpen = func(name string, args ...string) error { return nil }

	if err := runWebLoginCLI(srv.URL, nil); err != nil {
		t.Fatalf("expected nil error for approved flow, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 RPC calls (start + wait), got %d", calls)
	}
}

func TestRunWebLoginRejectedFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "web.login.start") {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"nonce":"abc","url":"/web/login/abc"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"status":"rejected"}}`))
	}))
	t.Cleanup(srv.Close)

	origExec := execOpen
	t.Cleanup(func() { execOpen = origExec })
	execOpen = func(name string, args ...string) error { return nil }

	err := runWebLoginCLI(srv.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected rejection error, got %v", err)
	}
}

func TestRunWebLoginStartErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"server is shutting down"}}`))
	}))
	t.Cleanup(srv.Close)

	err := runWebLoginCLI(srv.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("expected start error, got %v", err)
	}
}

// configurePathHarness redirects config.DefaultPath() at a temp file and
// returns a loader for the post-write state. Used by the new-channel
// configure tests so the real user config is never touched.
func configurePathHarness(t *testing.T) func() config.Config {
	t.Helper()
	tmpPath := filepath.Join(t.TempDir(), "openclaw.json")
	t.Setenv("OPENCLAW_CONFIG_PATH", tmpPath)
	return func() config.Config {
		got, err := config.Load(tmpPath)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
}

func TestRunConfigureEmailSetsAllFields(t *testing.T) {
	load := configurePathHarness(t)
	cfg := config.Default()

	if err := runConfigureEmail(cfg, []string{"enable", "true"}); err != nil {
		t.Fatal(err)
	}
	got := load()
	if !got.Channels.Email.Enabled {
		t.Fatal("enable=true didn't persist")
	}

	if err := runConfigureEmail(got, []string{"host", "smtp.example.com"}); err != nil {
		t.Fatal(err)
	}
	got = load()
	if got.Channels.Email.Host != "smtp.example.com" {
		t.Fatalf("host: %q", got.Channels.Email.Host)
	}

	if err := runConfigureEmail(got, []string{"port", "465"}); err != nil {
		t.Fatal(err)
	}
	got = load()
	if got.Channels.Email.Port != 465 {
		t.Fatalf("port: %d", got.Channels.Email.Port)
	}

	// Each call must reload — every real CLI invocation is a fresh process,
	// so passing a stale cfg from before the previous save would discard
	// the previous field.
	if err := runConfigureEmail(load(), []string{"user", "bot@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := runConfigureEmail(load(), []string{"password", "app-pw-with-spaces"}); err != nil {
		t.Fatal(err)
	}
	if err := runConfigureEmail(load(), []string{"from", "bot@example.com"}); err != nil {
		t.Fatal(err)
	}
	got = load()
	if got.Channels.Email.Username != "bot@example.com" {
		t.Fatalf("user: %q", got.Channels.Email.Username)
	}
	if got.Channels.Email.Password != "app-pw-with-spaces" {
		// Password must NOT be trimmed (app-passwords sometimes have spaces).
		t.Fatalf("password trimmed unexpectedly: %q", got.Channels.Email.Password)
	}
	if got.Channels.Email.From != "bot@example.com" {
		t.Fatalf("from: %q", got.Channels.Email.From)
	}
}

func TestRunConfigureEmailRejectsBadPort(t *testing.T) {
	configurePathHarness(t)
	cfg := config.Default()
	if err := runConfigureEmail(cfg, []string{"port", "0"}); err == nil {
		t.Fatal("expected error for port=0")
	}
	if err := runConfigureEmail(cfg, []string{"port", "not-a-number"}); err == nil {
		t.Fatal("expected error for non-numeric port")
	}
	if err := runConfigureEmail(cfg, []string{"port", "70000"}); err == nil {
		t.Fatal("expected error for port out of range")
	}
}

func TestRunConfigureSignal(t *testing.T) {
	load := configurePathHarness(t)
	cfg := config.Default()

	if err := runConfigureSignal(cfg, []string{"enable", "true"}); err != nil {
		t.Fatal(err)
	}
	if err := runConfigureSignal(load(), []string{"baseurl", "http://127.0.0.1:8080"}); err != nil {
		t.Fatal(err)
	}
	if err := runConfigureSignal(load(), []string{"number", "+15551112222"}); err != nil {
		t.Fatal(err)
	}
	got := load()
	if !got.Channels.Signal.Enabled || got.Channels.Signal.BaseURL != "http://127.0.0.1:8080" ||
		got.Channels.Signal.Number != "+15551112222" {
		t.Fatalf("signal config not persisted correctly: %+v", got.Channels.Signal)
	}
}

func TestRunConfigureMatrix(t *testing.T) {
	load := configurePathHarness(t)
	cfg := config.Default()

	_ = runConfigureMatrix(cfg, []string{"enable", "true"})
	_ = runConfigureMatrix(load(), []string{"baseurl", "https://matrix.example.com"})
	_ = runConfigureMatrix(load(), []string{"token", "syt_xyz"})
	got := load()
	if !got.Channels.Matrix.Enabled || got.Channels.Matrix.BaseURL != "https://matrix.example.com" ||
		got.Channels.Matrix.AccessToken != "syt_xyz" {
		t.Fatalf("matrix config not persisted: %+v", got.Channels.Matrix)
	}
}

func TestRunConfigureMattermost(t *testing.T) {
	load := configurePathHarness(t)
	cfg := config.Default()

	_ = runConfigureMattermost(cfg, []string{"enable", "true"})
	_ = runConfigureMattermost(load(), []string{"baseurl", "https://mm.example.com"})
	_ = runConfigureMattermost(load(), []string{"token", "mm-tok"})
	got := load()
	if !got.Channels.Mattermost.Enabled || got.Channels.Mattermost.BaseURL != "https://mm.example.com" ||
		got.Channels.Mattermost.AccessToken != "mm-tok" {
		t.Fatalf("mattermost config not persisted: %+v", got.Channels.Mattermost)
	}
}

func TestRunConfigureUsageErrors(t *testing.T) {
	configurePathHarness(t)
	cfg := config.Default()
	cases := []struct {
		name string
		fn   func() error
	}{
		{"email no args", func() error { return runConfigureEmail(cfg, nil) }},
		{"signal no args", func() error { return runConfigureSignal(cfg, nil) }},
		{"matrix no args", func() error { return runConfigureMatrix(cfg, nil) }},
		{"mattermost no args", func() error { return runConfigureMattermost(cfg, nil) }},
		{"email bad subcmd", func() error { return runConfigureEmail(cfg, []string{"magic", "yes"}) }},
		{"signal bad subcmd", func() error { return runConfigureSignal(cfg, []string{"magic", "yes"}) }},
		{"matrix bad subcmd", func() error { return runConfigureMatrix(cfg, []string{"magic", "yes"}) }},
		{"mattermost bad subcmd", func() error { return runConfigureMattermost(cfg, []string{"magic", "yes"}) }},
		{"email enable non-bool", func() error { return runConfigureEmail(cfg, []string{"enable", "yes-maybe"}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestRunBackupMissingDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// No data dir created → backup should error with a clear pointer to onboard.
	err := runBackup(nil)
	if err == nil {
		t.Fatal("expected error when data dir missing")
	}
	if !strings.Contains(err.Error(), "onboard") {
		t.Fatalf("error should hint at onboard; got %v", err)
	}
}

func TestApplyOnboardOptionsZeroFlagsLeavesConfigAlone(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Provider = "anthropic"
	cfg.Gateway.AuthToken = "preserve-me"
	applyOnboardOptions(&cfg, onboardOptions{})
	if cfg.Agent.Provider != "anthropic" {
		t.Fatalf("provider mutated: %q", cfg.Agent.Provider)
	}
	if cfg.Gateway.AuthToken != "preserve-me" {
		t.Fatalf("auth mutated: %q", cfg.Gateway.AuthToken)
	}
}

func TestValidateGatewayChannelConfig_NewChannels(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(c *config.Config)
		wantError string
	}{
		{
			"email enabled without host",
			func(c *config.Config) { c.Channels.Email.Enabled = true },
			"email.host",
		},
		{
			"signal enabled without baseUrl",
			func(c *config.Config) {
				c.Channels.Signal.Enabled = true
				c.Channels.Signal.Number = "+15551112222"
			},
			"signal.baseUrl",
		},
		{
			"signal enabled without number",
			func(c *config.Config) {
				c.Channels.Signal.Enabled = true
				c.Channels.Signal.BaseURL = "http://localhost:8080"
			},
			"signal.number",
		},
		{
			"matrix enabled without baseUrl",
			func(c *config.Config) {
				c.Channels.Matrix.Enabled = true
				c.Channels.Matrix.AccessToken = "tok"
			},
			"matrix.baseUrl",
		},
		{
			"matrix enabled without accessToken",
			func(c *config.Config) {
				c.Channels.Matrix.Enabled = true
				c.Channels.Matrix.BaseURL = "https://example.com"
			},
			"matrix.accessToken",
		},
		{
			"mattermost enabled without baseUrl",
			func(c *config.Config) {
				c.Channels.Mattermost.Enabled = true
				c.Channels.Mattermost.AccessToken = "tok"
			},
			"mattermost.baseUrl",
		},
		{
			"mattermost enabled without accessToken",
			func(c *config.Config) {
				c.Channels.Mattermost.Enabled = true
				c.Channels.Mattermost.BaseURL = "https://example.com"
			},
			"mattermost.accessToken",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.mutate(&cfg)
			err := validateGatewayChannelConfig(cfg)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error %q should mention %q", err.Error(), tc.wantError)
			}
		})
	}
}

func TestValidateGatewayChannelConfig_NewChannelsHappyPath(t *testing.T) {
	cfg := config.Default()
	cfg.Channels.Email = config.EmailChannelConfig{Enabled: true, Host: "smtp.example.com", Port: 587, Username: "x", Password: "y"}
	cfg.Channels.Signal = config.SignalChannelConfig{Enabled: true, BaseURL: "http://localhost:8080", Number: "+15551112222"}
	cfg.Channels.Matrix = config.MatrixChannelConfig{Enabled: true, BaseURL: "https://matrix.example.com", AccessToken: "syt_x"}
	cfg.Channels.Mattermost = config.MattermostChannelConfig{Enabled: true, BaseURL: "https://mm.example.com", AccessToken: "tok"}
	if err := validateGatewayChannelConfig(cfg); err != nil {
		t.Fatalf("happy path should validate: %v", err)
	}
}

func TestValidateGatewayChannelConfig_WhatsAppVerifyToken(t *testing.T) {
	t.Run("enabled without verify token", func(t *testing.T) {
		cfg := config.Default()
		cfg.Channels.WhatsApp.Enabled = true
		cfg.Channels.WhatsApp.VerifyToken = ""
		err := validateGatewayChannelConfig(cfg)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "verify token") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("enabled with verify token", func(t *testing.T) {
		cfg := config.Default()
		cfg.Channels.WhatsApp.Enabled = true
		cfg.Channels.WhatsApp.VerifyToken = "secret"
		if err := validateGatewayChannelConfig(cfg); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("disabled without verify token ok", func(t *testing.T) {
		cfg := config.Default()
		cfg.Channels.WhatsApp.Enabled = false
		cfg.Channels.WhatsApp.VerifyToken = ""
		if err := validateGatewayChannelConfig(cfg); err != nil {
			t.Fatal(err)
		}
	})
}

func TestConfigureSetAgentProviderSyncsModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	t.Setenv("OPENCLAW_CONFIG_PATH", path)

	cfg := config.Default()
	cfg.Agent.Provider = "echo"
	cfg.Agent.Model = "echo"
	cfg.Providers.OpenAI.Model = "gpt-4o"
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if err := runConfigure(loaded, []string{"set-agent-provider", "openai"}); err != nil {
		t.Fatal(err)
	}
	after, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if after.Agent.Provider != "openai" {
		t.Fatalf("provider %q", after.Agent.Provider)
	}
	if after.Agent.Model != "gpt-4o" {
		t.Fatalf("model %q want gpt-4o", after.Agent.Model)
	}

	if err := runConfigure(after, []string{"set-agent-provider", "echo"}); err != nil {
		t.Fatal(err)
	}
	afterEcho, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if afterEcho.Agent.Model != "echo" {
		t.Fatalf("echo model %q", afterEcho.Agent.Model)
	}

	cfg2 := config.Default()
	cfg2.Providers.Anthropic.Model = "claude-test-model"
	if err := config.Save(path, cfg2); err != nil {
		t.Fatal(err)
	}
	loaded2, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if err := runConfigure(loaded2, []string{"set-agent-provider", "anthropic"}); err != nil {
		t.Fatal(err)
	}
	afterAnthropic, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if afterAnthropic.Agent.Model != "claude-test-model" {
		t.Fatalf("anthropic model %q", afterAnthropic.Agent.Model)
	}
}

func TestParseBoolArg(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
		ok   bool
	}{
		{"true", true, true},
		{"TRUE", true, true},
		{"yes", true, true},
		{"1", true, true},
		{"on", true, true},
		{"false", false, true},
		{"no", false, true},
		{"0", false, true},
		{"off", false, true},
		{"maybe", false, false},
		{"", false, false},
	} {
		got, err := parseBoolArg(tc.in)
		if tc.ok {
			if err != nil {
				t.Fatalf("%q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("%q: got %v want %v", tc.in, got, tc.want)
			}
		} else {
			if err == nil {
				t.Fatalf("%q: expected error", tc.in)
			}
		}
	}
}

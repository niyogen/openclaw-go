package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonUnitPathPerOS(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	cases := map[string]string{
		"linux":   filepath.Join(home, ".config", "systemd", "user", "openclaw.service"),
		"darwin":  filepath.Join(home, "Library", "LaunchAgents", "openclaw.plist"),
		"windows": filepath.Join(home, "AppData", "Local", "openclaw", "openclaw.bat"),
	}
	for osName, want := range cases {
		got, err := daemonUnitPath(osName)
		if err != nil {
			t.Fatalf("%s: %v", osName, err)
		}
		if got != want {
			t.Fatalf("%s: got %s want %s", osName, got, want)
		}
	}
	if _, err := daemonUnitPath("plan9"); err == nil {
		t.Fatal("expected error for unsupported OS")
	}
}

func TestSystemdUserUnitShape(t *testing.T) {
	got := systemdUserUnit("/usr/local/bin/openclaw")
	for _, want := range []string{
		"[Unit]",
		"Description=OpenClaw-Go gateway",
		"ExecStart=/usr/local/bin/openclaw gateway run",
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("unit missing %q\n---\n%s", want, got)
		}
	}
}

func TestLaunchdPlistShape(t *testing.T) {
	path := filepath.Join("/Users", "u", "Library", "LaunchAgents", "openclaw.plist")
	got := launchdPlist("/usr/local/bin/openclaw", path)
	for _, want := range []string{
		`<key>Label</key>`,
		`<string>openclaw</string>`,
		`<string>/usr/local/bin/openclaw</string>`,
		`<string>gateway</string>`,
		`<key>RunAtLoad</key>`,
		`<key>KeepAlive</key>`,
		// Log path lives under ~/Library/Logs.
		`/Users/u/Library/Logs/openclaw.out.log`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plist missing %q\n---\n%s", want, got)
		}
	}
}

func TestRunDaemonInstallLinuxWritesUnitFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if err := runDaemonInstall("linux"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "openclaw.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "ExecStart=") {
		t.Fatalf("unit file content unexpected: %s", got)
	}
}

func TestRunDaemonInstallDarwinWritesPlist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if err := runDaemonInstall("darwin"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", "openclaw.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "<plist") {
		t.Fatalf("plist content unexpected: %s", got)
	}
}

func TestRunDaemonInstallWindowsRejects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	err := runDaemonInstall("windows")
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected 'not supported' error on Windows; got %v", err)
	}
}

func TestRunDaemonUninstallNoOpWhenMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// No file present; should succeed without error.
	if err := runDaemonUninstall("linux"); err != nil {
		t.Fatal(err)
	}
}

func TestRunDaemonUninstallRemovesExistingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if err := runDaemonInstall("linux"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "systemd", "user", "openclaw.service")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pre-uninstall stat: %v", err)
	}
	if err := runDaemonUninstall("linux"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unit file should be gone after uninstall; got err=%v", err)
	}
}

func TestRunDaemonUsageErrors(t *testing.T) {
	if err := runDaemon(nil); err == nil {
		t.Fatal("expected usage error with no args")
	}
	if err := runDaemon([]string{"unknown"}); err == nil {
		t.Fatal("expected unknown-subcommand error")
	}
}

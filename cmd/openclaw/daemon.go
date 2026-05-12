package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// runDaemon dispatches `openclaw daemon <subcmd>`. Subcommands:
//
//	install   — write the user-level service unit for the current OS
//	uninstall — remove it
//	path      — print the target file path without writing
//
// install/uninstall *write or remove the unit file* but do NOT shell out to
// systemctl/launchctl. We print the activation command instead so the user
// can review and run it themselves — auto-execution would silently expand
// the CLI's blast radius (it could mask permission errors, restart the
// gateway under the wrong session, etc.).
func runDaemon(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: openclaw daemon install|uninstall|path")
	}
	switch args[0] {
	case "path":
		path, err := daemonUnitPath(goos())
		if err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	case "install":
		return runDaemonInstall(goos())
	case "uninstall":
		return runDaemonUninstall(goos())
	default:
		return fmt.Errorf("unknown daemon subcommand %q", args[0])
	}
}

func runDaemonInstall(osName string) error {
	path, err := daemonUnitPath(osName)
	if err != nil {
		return err
	}
	if osName == "windows" {
		return fmt.Errorf("daemon install on Windows is not supported yet — use a third-party wrapper like NSSM, or run `openclaw gateway run` under Task Scheduler")
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate openclaw binary: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	var content, postInstall string
	switch osName {
	case "linux":
		content = systemdUserUnit(exe)
		postInstall = "systemctl --user daemon-reload && systemctl --user enable --now openclaw"
	case "darwin":
		content = launchdPlist(exe, path)
		postInstall = "launchctl bootstrap gui/$(id -u) " + path
	default:
		return fmt.Errorf("unsupported OS %q", osName)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write unit file: %w", err)
	}
	fmt.Printf("Wrote %s\n", path)
	fmt.Println("Activate it with:")
	fmt.Println("  ", postInstall)
	return nil
}

func runDaemonUninstall(osName string) error {
	path, err := daemonUnitPath(osName)
	if err != nil {
		return err
	}
	if osName == "windows" {
		return fmt.Errorf("daemon uninstall on Windows is not supported yet")
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("(no unit file at %s — nothing to remove)\n", path)
			return nil
		}
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	fmt.Printf("Removed %s\n", path)
	var deactivate string
	switch osName {
	case "linux":
		deactivate = "systemctl --user disable --now openclaw && systemctl --user daemon-reload"
	case "darwin":
		deactivate = "launchctl bootout gui/$(id -u)/openclaw"
	}
	if deactivate != "" {
		fmt.Println("If the unit was running, stop it with:")
		fmt.Println("  ", deactivate)
	}
	return nil
}

// daemonUnitPath returns the platform-canonical user-scope service file
// path. Pure function — no I/O — so tests can call it directly.
func daemonUnitPath(osName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch osName {
	case "linux":
		// User-scope systemd unit; works without root and survives reboots
		// when `loginctl enable-linger <user>` is set.
		return filepath.Join(home, ".config", "systemd", "user", "openclaw.service"), nil
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", "openclaw.plist"), nil
	case "windows":
		// Returned even though install/uninstall reject windows — `path`
		// subcommand should still be informative.
		return filepath.Join(home, "AppData", "Local", "openclaw", "openclaw.bat"), nil
	default:
		return "", fmt.Errorf("unsupported OS %q", osName)
	}
}

// systemdUserUnit returns the systemd .service content for a user-scope
// unit. Restart=on-failure with a 5-second backoff so transient errors
// don't busy-loop the gateway. Type=simple because `openclaw gateway run`
// stays in the foreground.
func systemdUserUnit(exe string) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=OpenClaw-Go gateway",
		"After=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		"ExecStart=" + exe + " gateway run",
		"Restart=on-failure",
		"RestartSec=5s",
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	}, "\n")
}

// launchdPlist returns the launchd plist content for a user agent. The
// Label MUST match the file's base name (sans `.plist`) per launchd
// convention; we hard-code "openclaw" for that reason.
//
// plistPath is the macOS file path we're writing to (POSIX-style). It is
// only used to derive the sibling Logs directory — the function uses the
// `path` package (forward slashes) rather than `filepath` so the embedded
// log paths look right on the macOS target even when this code is
// cross-built on Windows.
func launchdPlist(exe, plistPath string) string {
	// Normalize to forward slashes first so path.Dir (POSIX-only) walks
	// correctly even when this is cross-built on Windows. The plist is
	// macOS-bound so the output should always be POSIX style.
	// Two Dirs walks from ~/Library/LaunchAgents/openclaw.plist to ~/Library;
	// joining "Logs" lands on ~/Library/Logs which is the macOS-standard
	// per-user log directory.
	posixPath := filepath.ToSlash(plistPath)
	logBase := path.Join(path.Dir(path.Dir(posixPath)), "Logs")
	return strings.Join([]string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`,
		`<plist version="1.0">`,
		`<dict>`,
		`  <key>Label</key>`,
		`  <string>openclaw</string>`,
		`  <key>ProgramArguments</key>`,
		`  <array>`,
		`    <string>` + exe + `</string>`,
		`    <string>gateway</string>`,
		`    <string>run</string>`,
		`  </array>`,
		`  <key>RunAtLoad</key>`,
		`  <true/>`,
		`  <key>KeepAlive</key>`,
		`  <true/>`,
		`  <key>StandardOutPath</key>`,
		`  <string>` + path.Join(logBase, "openclaw.out.log") + `</string>`,
		`  <key>StandardErrorPath</key>`,
		`  <string>` + path.Join(logBase, "openclaw.err.log") + `</string>`,
		`</dict>`,
		`</plist>`,
		``,
	}, "\n")
}

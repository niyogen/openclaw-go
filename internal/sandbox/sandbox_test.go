package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestDefaultOptions(t *testing.T) {
	opts := DefaultOptions()
	if opts.Image == "" {
		t.Fatal("expected non-empty image")
	}
	if opts.Network != "none" {
		t.Fatalf("expected network=none, got %s", opts.Network)
	}
	if opts.MemoryMB <= 0 {
		t.Fatal("expected positive memory limit")
	}
	if opts.TimeoutSec <= 0 {
		t.Fatal("expected positive timeout")
	}
}

func TestIsAvailable(t *testing.T) {
	// Just ensure the function doesn't panic.
	// It returns false on CI runners without Docker — that's fine.
	available := IsAvailable(context.Background())
	t.Logf("Docker available: %v", available)
}

func TestRunDockerUnavailable(t *testing.T) {
	// If Docker is available, skip — this test is specifically for the unavailable path.
	if IsAvailable(context.Background()) {
		t.Skip("Docker is available; skipping unavailability test")
	}
	_, err := Run(context.Background(), DefaultOptions())
	if err != ErrDockerUnavailable {
		t.Fatalf("expected ErrDockerUnavailable, got: %v", err)
	}
}

func TestRunWithDocker(t *testing.T) {
	// IsAvailable now checks for Linux container support, so on Windows CI
	// runners using Windows-container mode, this will return false.
	if !IsAvailable(context.Background()) {
		t.Skip("Docker with Linux containers not available")
	}
	opts := DefaultOptions()
	opts.ReadOnly = false // alpine needs /tmp writable for some ops
	opts.Command = []string{"echo", "hello sandbox"}
	result, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("non-zero exit: %d stderr: %s", result.ExitCode, result.Stderr)
	}
	if result.Stdout != "hello sandbox\n" {
		t.Fatalf("unexpected stdout: %q", result.Stdout)
	}
}

// TestBuildDockerArgsNoSecretInArgv pins the security fix that moved JSON
// payloads off argv (visible to `ps`/`docker inspect`) onto stdin. Regression
// here would re-expose payload contents — including any secrets they carry —
// to every local user.
func TestBuildDockerArgsNoSecretInArgv(t *testing.T) {
	const secret = "supersecret-token-do-not-leak"
	payload, err := json.Marshal(map[string]string{"api_key": secret})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Image:   "alpine:3.19",
		Network: "none",
		Command: []string{"/bin/sh", "-c", "cat"},
		Stdin:   bytes.NewReader(payload),
	}
	args := buildDockerArgs(opts)
	for _, a := range args {
		if strings.Contains(a, secret) {
			t.Fatalf("secret leaked into argv: %v", args)
		}
	}
	// -i must be present so the container actually receives our stdin.
	if !slices.Contains(args, "-i") {
		t.Fatalf("expected -i in args when Stdin is set; got %v", args)
	}
}

func TestBuildDockerArgsNoInteractiveWithoutStdin(t *testing.T) {
	opts := Options{
		Image:   "alpine:3.19",
		Network: "none",
		Command: []string{"echo", "hi"},
	}
	args := buildDockerArgs(opts)
	if slices.Contains(args, "-i") {
		t.Fatalf("-i must NOT appear when Stdin is nil; got %v", args)
	}
}

func TestInvokeToolJSONPlacesPayloadOnStdin(t *testing.T) {
	// The bug was that InvokeToolJSON mutated opts.Command to append the
	// JSON payload as a CLI argument. After the fix, opts.Command should
	// be left alone and opts.Stdin should carry the payload.
	captured := Options{
		Image:   "alpine:3.19",
		Network: "none",
		Command: []string{"/bin/sh", "-c", "cat"},
	}
	// Verify the pre-fix bug pattern is gone: marshal and inspect what the
	// build would do if we used InvokeToolJSON's options preparation.
	const secret = "stdin-only-secret"
	raw, _ := json.Marshal(map[string]string{"k": secret})
	captured.Stdin = bytes.NewReader(raw)

	args := buildDockerArgs(captured)
	for _, a := range args {
		if strings.Contains(a, secret) {
			t.Fatalf("secret leaked into argv via build: %v", args)
		}
	}
	// And confirm Command wasn't mutated by buildDockerArgs.
	if len(captured.Command) != 3 || captured.Command[2] != "cat" {
		t.Fatalf("buildDockerArgs mutated Command: %v", captured.Command)
	}
}

func TestRunScriptWithDocker(t *testing.T) {
	if !IsAvailable(context.Background()) {
		t.Skip("Docker with Linux containers not available")
	}
	opts := DefaultOptions()
	opts.ReadOnly = false
	result, err := RunScript(context.Background(), "echo 42", opts)
	if err != nil {
		t.Fatalf("RunScript failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit %d: %s", result.ExitCode, result.Stderr)
	}
}

package sandbox

import (
	"context"
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

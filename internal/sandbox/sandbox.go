// Package sandbox provides Docker-based isolated execution for agent tools.
// Each tool invocation through the sandbox runs inside a throwaway container
// with configurable resource limits, network isolation, and a read-only
// filesystem (except a scratch volume).
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Options configure a single sandboxed run.
type Options struct {
	// Image is the Docker image to use. Default: "alpine:3.19"
	Image string

	// Command is the entrypoint command to run inside the container.
	Command []string

	// Env is a list of "KEY=VALUE" environment variables.
	Env []string

	// Network controls network access. "none" (default) fully isolates.
	// Use "bridge" to allow outbound internet access.
	Network string

	// MemoryMB caps memory usage (0 = no limit, recommended: 256).
	MemoryMB int

	// CPUs is the CPU quota (0 = no limit, e.g. 0.5 for half a core).
	CPUs float64

	// TimeoutSec is the maximum wall-clock time for the run (default: 30).
	TimeoutSec int

	// ReadOnly mounts the container filesystem as read-only.
	ReadOnly bool

	// Stdin, if non-nil, is piped into the container via `docker run -i`.
	// Use this to deliver payloads (e.g. JSON arguments) without exposing
	// them in argv — argv is visible to anyone who can read `ps` output or
	// `docker inspect`, so secrets must travel through stdin instead.
	Stdin io.Reader
}

// Result is the output of a sandboxed run.
type Result struct {
	Stdout   string        `json:"stdout"`
	Stderr   string        `json:"stderr"`
	ExitCode int           `json:"exitCode"`
	Duration time.Duration `json:"duration"`
}

// DefaultOptions returns a safe default configuration.
func DefaultOptions() Options {
	return Options{
		Image:      "alpine:3.19",
		Network:    "none",
		MemoryMB:   256,
		CPUs:       0.5,
		TimeoutSec: 30,
		ReadOnly:   true,
	}
}

// Run executes cmd inside a throwaway Docker container and returns stdout/stderr.
// If Docker is not available or does not support Linux containers it returns ErrDockerUnavailable.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if !IsAvailable(ctx) {
		return nil, ErrDockerUnavailable
	}

	if opts.Image == "" {
		opts.Image = "alpine:3.19"
	}
	if opts.TimeoutSec <= 0 {
		opts.TimeoutSec = 30
	}
	if opts.Network == "" {
		opts.Network = "none"
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(opts.TimeoutSec)*time.Second)
	defer cancel()

	args := buildDockerArgs(opts)

	start := time.Now()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(runCtx, "docker", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if opts.Stdin != nil {
		cmd.Stdin = opts.Stdin
	}

	err := cmd.Run()
	dur := time.Since(start)

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return &Result{
				Stdout:   stdout.String(),
				Stderr:   "timeout: container exceeded time limit",
				ExitCode: -1,
				Duration: dur,
			}, nil
		} else {
			return nil, fmt.Errorf("docker run: %w", err)
		}
	}
	return &Result{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		Duration: dur,
	}, nil
}

// buildDockerArgs assembles the `docker run` argv from Options. Factored out
// so tests can assert security-relevant invariants (no secrets in argv, -i
// added when stdin is in use) without spawning docker.
func buildDockerArgs(opts Options) []string {
	args := []string{"run", "--rm", "--network=" + opts.Network}
	if opts.MemoryMB > 0 {
		args = append(args, fmt.Sprintf("--memory=%dm", opts.MemoryMB))
		args = append(args, fmt.Sprintf("--memory-swap=%dm", opts.MemoryMB))
	}
	if opts.CPUs > 0 {
		args = append(args, fmt.Sprintf("--cpus=%.2f", opts.CPUs))
	}
	if opts.ReadOnly {
		args = append(args, "--read-only")
	}
	if opts.Stdin != nil {
		// -i keeps STDIN open inside the container so the caller's payload
		// is actually delivered to the entrypoint, not swallowed by docker.
		args = append(args, "-i")
	}
	for _, e := range opts.Env {
		args = append(args, "-e", e)
	}
	args = append(args, opts.Image)
	args = append(args, opts.Command...)
	return args
}

// ErrDockerUnavailable is returned when the Docker daemon is not reachable.
var ErrDockerUnavailable = errors.New("docker daemon is not available")

func checkDocker(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, "docker", "info", "--format", "{{.ServerVersion}}")
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

// IsAvailable returns true if Docker is reachable AND can run Linux containers.
// On Windows, Docker may be running in Windows-container mode which cannot
// run Linux images like alpine — in that case we return false.
func IsAvailable(ctx context.Context) bool {
	if checkDocker(ctx) != nil {
		return false
	}
	// Verify Linux container support by checking the OS type reported by Docker.
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, "docker", "info", "--format", "{{.OSType}}")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	osType := strings.TrimSpace(strings.ToLower(string(out)))
	return osType == "linux"
}

// RunScript is a convenience wrapper that executes a shell script string
// inside the sandbox (using /bin/sh).
func RunScript(ctx context.Context, script string, opts Options) (*Result, error) {
	opts.Command = []string{"/bin/sh", "-c", script}
	return Run(ctx, opts)
}

// InvokeToolJSON sends a JSON payload to a command inside the sandbox via
// STDIN and returns the parsed JSON output.  The command must accept JSON on
// stdin and write JSON to stdout. The payload is intentionally NOT placed in
// argv — argv leaks to `ps` and `docker inspect`, so anything sensitive in
// the payload would be exposed to other local users.
func InvokeToolJSON(ctx context.Context, payload any, opts Options) (any, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	opts.Stdin = bytes.NewReader(raw)
	result, err := Run(ctx, opts)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("sandbox exited %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	var out any
	if err := json.Unmarshal([]byte(result.Stdout), &out); err != nil {
		return result.Stdout, nil // return raw string if not JSON
	}
	return out, nil
}

// Package cronstore provides a simple cron-job registry and scheduler.
package cronstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"openclaw-go/internal/fileutil"
)

// Job is a recurring task definition.
type Job struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Schedule     string     `json:"schedule"` // e.g. "@every 5m", "hourly", "daily"
	Command      string     `json:"command"`
	Enabled      bool       `json:"enabled"`
	LastRunAt    *time.Time `json:"lastRunAt,omitempty"`
	LastRunError string     `json:"lastRunError,omitempty"`
	RunCount     int        `json:"runCount"`
	CreatedAt    time.Time  `json:"createdAt"`
}

// Run records one execution of a job.
type Run struct {
	JobID     string    `json:"jobId"`
	StartedAt time.Time `json:"startedAt"`
	Duration  string    `json:"duration"`
	ExitCode  int       `json:"exitCode"`
	Output    string    `json:"output,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// RunFn is the function called when a job fires.
type RunFn func(ctx context.Context, job Job)

// Store holds cron jobs and drives a simple ticker-based scheduler.
type Store struct {
	mu      sync.Mutex
	jobs    map[string]*Job
	runs    []Run // in-memory run history (capped at 500)
	path    string
	running map[string]bool // tracks jobs currently executing (not persisted)
}

// New opens (or creates) a cron store backed by path.
func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: path, jobs: map[string]*Job{}, runs: []Run{}, running: map[string]bool{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Add creates or replaces a job.
func (s *Store) Add(job Job) error {
	if strings.TrimSpace(job.ID) == "" {
		return errors.New("job id is required")
	}
	if strings.TrimSpace(job.Schedule) == "" {
		return errors.New("job schedule is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job.CreatedAt = time.Now().UTC()
	s.jobs[job.ID] = &job
	return s.saveLocked()
}

// Remove deletes a job by id; returns false if not found.
func (s *Store) Remove(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; !ok {
		return false, nil
	}
	delete(s.jobs, id)
	return true, s.saveLocked()
}

// List returns all jobs.
func (s *Store) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, *j)
	}
	return out
}

// Runs returns the last N run records (most recent last).
func (s *Store) Runs(jobID string, limit int) []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Run
	for _, r := range s.runs {
		if jobID != "" && r.JobID != jobID {
			continue
		}
		out = append(out, r)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func (s *Store) recordRun(run Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = append(s.runs, run)
	if len(s.runs) > 500 {
		s.runs = s.runs[len(s.runs)-500:]
	}
}

// ExecuteJob runs the job's Command in a subprocess and records the result.
func (s *Store) ExecuteJob(ctx context.Context, job Job) Run {
	start := time.Now()
	run := Run{JobID: job.ID, StartedAt: start}
	if strings.TrimSpace(job.Command) == "" {
		run.Error = "no command configured"
		run.Duration = time.Since(start).String()
		s.recordRun(run)
		return run
	}
	jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Choose shell based on OS: sh on Unix, cmd on Windows.
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(jobCtx, "cmd", "/C", job.Command) //nolint:gosec
	} else {
		cmd = exec.CommandContext(jobCtx, "sh", "-c", job.Command) //nolint:gosec
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	run.Duration = time.Since(start).String()
	run.Output = strings.TrimSpace(out.String())
	if err != nil {
		run.Error = err.Error()
		if cmd.ProcessState != nil {
			run.ExitCode = cmd.ProcessState.ExitCode()
		} else {
			run.ExitCode = -1
		}
	}
	s.recordRun(run)
	// Update job metadata.
	s.mu.Lock()
	if j, ok := s.jobs[job.ID]; ok {
		now := time.Now().UTC()
		j.LastRunAt = &now
		j.RunCount++
		if run.Error != "" {
			j.LastRunError = run.Error
		} else {
			j.LastRunError = ""
		}
		_ = s.saveLocked()
	}
	s.mu.Unlock()
	return run
}

// TryLockRunning marks a job as running; returns false if already running.
// Used by manual triggers (e.g. cron.run RPC) to participate in the same
// overlap guard as the scheduler.
func (s *Store) TryLockRunning(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[id] {
		return false
	}
	s.running[id] = true
	return true
}

// UnlockRunning clears the running flag for a job.
func (s *Store) UnlockRunning(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, id)
}

// Get returns a single job by id.
func (s *Store) Get(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// scheduleInterval parses a simple schedule string into a duration.
func scheduleInterval(schedule string) (time.Duration, error) {
	s := strings.TrimSpace(strings.ToLower(schedule))
	switch s {
	case "hourly", "@hourly":
		return time.Hour, nil
	case "daily", "@daily":
		return 24 * time.Hour, nil
	case "weekly", "@weekly":
		return 7 * 24 * time.Hour, nil
	}
	if rest, ok := strings.CutPrefix(s, "@every "); ok {
		d, err := time.ParseDuration(rest)
		if err != nil {
			return 0, fmt.Errorf("invalid @every schedule %q: %w", schedule, err)
		}
		return d, nil
	}
	return 0, fmt.Errorf("unsupported schedule %q (use @every <duration>, hourly, daily, weekly)", schedule)
}

// StartScheduler runs all enabled jobs on their schedule until ctx is cancelled.
func (s *Store) StartScheduler(ctx context.Context, runFn RunFn) {
	go s.schedulerLoop(ctx, runFn)
}

func (s *Store) schedulerLoop(ctx context.Context, runFn RunFn) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			s.mu.Lock()
			jobs := make([]Job, 0, len(s.jobs))
			for _, j := range s.jobs {
				jobs = append(jobs, *j)
			}
			s.mu.Unlock()

			for _, job := range jobs {
				if !job.Enabled {
					continue
				}
				interval, err := scheduleInterval(job.Schedule)
				if err != nil {
					continue
				}
				if job.LastRunAt != nil && now.Sub(*job.LastRunAt) < interval {
					continue
				}
				// Skip if a previous execution of this job is still running.
				s.mu.Lock()
				if s.running[job.ID] {
					s.mu.Unlock()
					continue
				}
				s.running[job.ID] = true
				s.mu.Unlock()

				go func(j Job) {
					defer func() {
						s.mu.Lock()
						delete(s.running, j.ID)
						s.mu.Unlock()
					}()
					if runFn != nil {
						runFn(ctx, j)
					}
				}(job)
			}
		}
	}
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	var jobs []Job
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return fmt.Errorf("cronstore: corrupt data file %s: %w", s.path, err)
	}
	for i := range jobs {
		j := jobs[i]
		s.jobs[j.ID] = &j
	}
	return nil
}

func (s *Store) saveLocked() error {
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, *j)
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFile(s.path, raw, 0o600)
}

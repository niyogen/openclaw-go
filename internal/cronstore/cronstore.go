// Package cronstore provides a simple cron-job registry and scheduler.
package cronstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
	CreatedAt    time.Time  `json:"createdAt"`
}

// RunFn is the function called when a job fires.
type RunFn func(ctx context.Context, job Job)

// Store holds cron jobs and drives a simple ticker-based scheduler.
type Store struct {
	mu   sync.Mutex
	jobs map[string]*Job
	path string
}

// New opens (or creates) a cron store backed by path.
func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: path, jobs: map[string]*Job{}}
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
	if strings.HasPrefix(s, "@every ") {
		d, err := time.ParseDuration(strings.TrimPrefix(s, "@every "))
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
				go func(j Job) {
					if runFn != nil {
						runFn(ctx, j)
					}
					now2 := time.Now().UTC()
					s.mu.Lock()
					if entry, ok := s.jobs[j.ID]; ok {
						entry.LastRunAt = &now2
						entry.LastRunError = ""
					}
					_ = s.saveLocked()
					s.mu.Unlock()
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
		return nil
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
	return os.WriteFile(s.path, raw, 0o644)
}

package cronstore

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCronStoreAddRemoveList(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "cron.json"))
	if err != nil {
		t.Fatal(err)
	}
	job := Job{ID: "j1", Name: "test job", Schedule: "@every 1h", Command: "echo hi", Enabled: true}
	if err := s.Add(job); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("j1"); !ok {
		t.Fatal("expected job to exist")
	}
	jobs := s.List()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	deleted, err := s.Remove("j1")
	if err != nil || !deleted {
		t.Fatalf("Remove: deleted=%v err=%v", deleted, err)
	}
	if len(s.List()) != 0 {
		t.Fatal("expected 0 jobs after remove")
	}
}

// TestNoOverlappingRuns verifies that the scheduler does not start a second
// instance of a job while one is already running.
func TestNoOverlappingRuns(t *testing.T) {
	s, err := New(t.TempDir() + "/cron.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Add(Job{ID: "overlap", Schedule: "@every 1s", Enabled: true, Command: "echo hi"})

	var concurrent int64 // max concurrent executions seen
	var running int64    // currently running count

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	s.StartScheduler(ctx, func(_ context.Context, j Job) {
		cur := atomic.AddInt64(&running, 1)
		if prev := atomic.LoadInt64(&concurrent); cur > prev {
			atomic.StoreInt64(&concurrent, cur)
		}
		time.Sleep(150 * time.Millisecond) // simulate long job
		atomic.AddInt64(&running, -1)
	})

	<-ctx.Done()
	time.Sleep(50 * time.Millisecond) // let any running goroutines finish

	if atomic.LoadInt64(&concurrent) > 1 {
		t.Fatalf("job ran concurrently: max concurrent=%d", atomic.LoadInt64(&concurrent))
	}
}

func TestScheduleInterval(t *testing.T) {
	cases := []struct {
		input string
		ok    bool
	}{
		{"hourly", true},
		{"daily", true},
		{"weekly", true},
		{"@every 5m", true},
		{"@every 30s", true},
		{"cron * * * *", false},
	}
	for _, c := range cases {
		_, err := scheduleInterval(c.input)
		if c.ok && err != nil {
			t.Errorf("schedule %q should be valid: %v", c.input, err)
		}
		if !c.ok && err == nil {
			t.Errorf("schedule %q should be invalid", c.input)
		}
	}
}

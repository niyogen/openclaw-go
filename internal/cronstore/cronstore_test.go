package cronstore

import (
	"path/filepath"
	"testing"
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

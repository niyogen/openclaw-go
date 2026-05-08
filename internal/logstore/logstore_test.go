package logstore

import (
	"path/filepath"
	"testing"
)

func TestLogStoreAppendAndList(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "logs.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Append(LevelInfo, "test", "hello", nil)
	s.Append(LevelWarn, "test", "warning", nil)
	s.Append(LevelError, "other", "error msg", map[string]any{"key": "val"})

	all := s.List("", "", 0)
	if len(all) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(all))
	}

	warnings := s.List("warn", "", 0)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warn entry, got %d", len(warnings))
	}

	testComp := s.List("", "test", 0)
	if len(testComp) != 2 {
		t.Fatalf("expected 2 test-component entries, got %d", len(testComp))
	}

	limited := s.List("", "", 2)
	if len(limited) != 2 {
		t.Fatalf("expected limit 2, got %d", len(limited))
	}
}

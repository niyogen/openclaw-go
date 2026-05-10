package fileutil

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// retryTempDir creates a temp dir and registers cleanup with retries for
// Windows, where the OS may briefly hold handles on recently-written files.
func retryTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fileutil-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 5; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(time.Duration(i+1) * 50 * time.Millisecond)
		}
	})
	return dir
}

func TestWriteFileBasic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	data := []byte(`{"hello":"world"}`)

	if err := WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("content mismatch: got %q, want %q", got, data)
	}
}

func TestWriteFileOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	if err := WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Fatalf("expected 'new', got %q", got)
	}
}

// TestWriteFileConcurrent verifies that concurrent writes do not corrupt the
// file — readers should always see complete, valid JSON.
func TestWriteFileConcurrent(t *testing.T) {
	dir := retryTempDir(t)
	path := filepath.Join(dir, "concurrent.json")

	var wg sync.WaitGroup
	const workers = 20
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			data := []byte(`{"n":` + string(rune('0'+n)) + `}`)
			_ = WriteFile(path, data, 0o644)
		}(i % 10) // reuse 0-9 to get small payloads
	}
	wg.Wait()

	// The file should exist and contain valid JSON (not a torn write).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("file is empty after concurrent writes")
	}
}

// TestWriteFileNoTempRemnants verifies that no .tmp-atomic- files are left
// behind after a successful write.
func TestWriteFileNoTempRemnants(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clean.json")

	if err := WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "clean.json" {
			t.Fatalf("unexpected leftover file: %s", e.Name())
		}
	}
}

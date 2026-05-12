//go:build !windows

package fileutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFilePermsHonored verifies that fileutil.WriteFile applies the
// requested mode to the final file on POSIX. This pins the 0o600 contract
// the stores rely on (sessions, hookstore, cronstore, topology, agents,
// logstore, config) so the security tightening can't regress unnoticed.
//
// Linux-only because Windows reports file modes via emulated bits that do
// not reflect real ACL permissions.
func TestWriteFilePermsHonored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perm.json")

	if err := WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("perm after create: got %o, want 0o600", got)
	}

	// Overwrite at a different mode and confirm the new mode sticks —
	// the atomic-rename path replaces the file, so the new mode wins.
	if err := WriteFile(path, []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("perm after overwrite: got %o, want 0o640", got)
	}
}

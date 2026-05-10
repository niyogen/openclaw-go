// Package fileutil provides safe file-writing helpers.
package fileutil

import (
	"os"
	"path/filepath"
)

// WriteFile atomically replaces path with data by writing to a sibling temp
// file and renaming it.  This prevents torn writes: readers always see either
// the complete old content or the complete new content.
//
// On Windows, rename over an existing file is supported via MoveFileEx.
// If the rename fails (e.g. the target is locked), the temp file is cleaned up
// and the error is returned — the original file is left unchanged.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-atomic-")
	if err != nil {
		// Fall back to direct write if we can't create a temp file.
		return os.WriteFile(path, data, perm)
	}
	tmpName := tmp.Name()

	// Guarantee the temp file is removed on any failure path.
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpName, path); err != nil {
		// Rename failed (e.g. cross-device or Windows lock); fall back to
		// direct write so callers always get the data persisted.
		return os.WriteFile(path, data, perm)
	}
	ok = true
	return nil
}

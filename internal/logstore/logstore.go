// Package logstore provides a rotating, in-memory + file-backed event log.
package logstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"openclaw-go/internal/fileutil"
)

// Level is the log entry severity.
type Level string

const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
	LevelDebug Level = "debug"
)

// Entry is a single log record.
type Entry struct {
	ID        uint64         `json:"id"`
	Time      time.Time      `json:"time"`
	Level     Level          `json:"level"`
	Component string         `json:"component"`
	Message   string         `json:"message"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// Store is an append-only log store capped at maxEntries.
type Store struct {
	mu         sync.Mutex
	entries    []Entry
	seq        uint64
	maxEntries int
	path       string
}

const defaultMax = 2000

// New opens (or creates) a log store backed by path.
func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: path, maxEntries: defaultMax}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Append adds a log entry. It returns an error if the entry cannot be
// persisted to disk; the in-memory log always reflects the append.
func (s *Store) Append(level Level, component, message string, meta map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	e := Entry{
		ID:        s.seq,
		Time:      time.Now().UTC(),
		Level:     level,
		Component: component,
		Message:   message,
		Meta:      meta,
	}
	s.entries = append(s.entries, e)
	if len(s.entries) > s.maxEntries {
		s.entries = s.entries[len(s.entries)-s.maxEntries:]
	}
	return s.saveLocked()
}

// List returns entries (newest last), optionally filtered by level/component.
func (s *Store) List(filterLevel, filterComponent string, limit int) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.entries {
		if filterLevel != "" && string(e.Level) != filterLevel {
			continue
		}
		if filterComponent != "" && e.Component != filterComponent {
			continue
		}
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
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
	var entries []Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("logstore: corrupt data file %s: %w", s.path, err)
	}
	s.entries = entries
	if len(s.entries) > 0 {
		s.seq = s.entries[len(s.entries)-1].ID
	}
	return nil
}

func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFile(s.path, raw, 0o600)
}

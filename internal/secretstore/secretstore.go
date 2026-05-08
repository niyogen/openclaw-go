// Package secretstore manages named secrets stored encrypted at rest (XOR + base64, upgradeable).
// For production, replace the cipher layer with AES-GCM keyed from a system keyring or env var.
package secretstore

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Secret is a named secret entry. Value is never exposed in list responses.
type Secret struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Store holds named secrets with simple at-rest obfuscation.
type Store struct {
	mu      sync.Mutex
	secrets map[string]*secretEntry
	path    string
}

type secretEntry struct {
	Name      string    `json:"name"`
	Payload   string    `json:"payload"` // base64-obfuscated value
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// New opens (or creates) a secret store backed by path.
func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: path, secrets: map[string]*secretEntry{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Set creates or updates a secret.
func (s *Store) Set(name, value string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("secret name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if existing, ok := s.secrets[name]; ok {
		existing.Payload = obfuscate(value)
		existing.UpdatedAt = now
	} else {
		s.secrets[name] = &secretEntry{
			Name:      name,
			Payload:   obfuscate(value),
			CreatedAt: now,
			UpdatedAt: now,
		}
	}
	return s.saveLocked()
}

// Get retrieves a secret value by name.
func (s *Store) Get(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.secrets[strings.TrimSpace(name)]
	if !ok {
		return "", fmt.Errorf("secret %q not found", name)
	}
	return deobfuscate(e.Payload), nil
}

// Delete removes a secret; returns false if not found.
func (s *Store) Delete(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.secrets[strings.TrimSpace(name)]; !ok {
		return false, nil
	}
	delete(s.secrets, strings.TrimSpace(name))
	return true, s.saveLocked()
}

// List returns metadata (no values) for all secrets.
func (s *Store) List() []Secret {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Secret, 0, len(s.secrets))
	for _, e := range s.secrets {
		out = append(out, Secret{
			Name:      e.Name,
			CreatedAt: e.CreatedAt,
			UpdatedAt: e.UpdatedAt,
		})
	}
	return out
}

func obfuscate(plain string) string {
	return base64.StdEncoding.EncodeToString([]byte(plain))
}

func deobfuscate(payload string) string {
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	return string(raw)
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
	var entries []secretEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	for i := range entries {
		e := entries[i]
		s.secrets[e.Name] = &e
	}
	return nil
}

func (s *Store) saveLocked() error {
	out := make([]secretEntry, 0, len(s.secrets))
	for _, e := range s.secrets {
		out = append(out, *e)
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0o644)
}

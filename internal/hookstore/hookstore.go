// Package hookstore manages event hooks: named callbacks triggered by gateway events.
package hookstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"openclaw-go/internal/fileutil"
)

// EventType is the gateway event a hook fires on.
type EventType string

const (
	EventMessageReceived  EventType = "message.received"
	EventMessageSent      EventType = "message.sent"
	EventSessionCreated   EventType = "session.created"
	EventAgentRunComplete EventType = "agent.run.complete"
	EventToolInvoked      EventType = "tool.invoked"
)

// HookType defines how the hook is dispatched.
type HookType string

const (
	HookTypeWebhook HookType = "webhook"
	HookTypeLog     HookType = "log"
)

// Hook is a registered event handler.
type Hook struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Event     EventType `json:"event"`
	Type      HookType  `json:"type"`
	Target    string    `json:"target"` // URL for webhook, label for log
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

// Store holds hooks and provides event dispatch.
type Store struct {
	mu     sync.Mutex
	hooks  map[string]*Hook
	path   string
	client *http.Client
}

// New opens (or creates) a hook store backed by path.
func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		path:   path,
		hooks:  map[string]*Hook{},
		client: &http.Client{Timeout: 10 * time.Second},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Add creates or replaces a hook.
func (s *Store) Add(hook Hook) error {
	if strings.TrimSpace(hook.ID) == "" {
		return errors.New("hook id is required")
	}
	if strings.TrimSpace(string(hook.Event)) == "" {
		return errors.New("hook event is required")
	}
	if hook.Type == "" {
		hook.Type = HookTypeLog
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hook.CreatedAt = time.Now().UTC()
	s.hooks[hook.ID] = &hook
	return s.saveLocked()
}

// Remove deletes a hook by id. Returns false if not found.
func (s *Store) Remove(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.hooks[id]; !ok {
		return false, nil
	}
	delete(s.hooks, id)
	return true, s.saveLocked()
}

// List returns all hooks.
func (s *Store) List() []Hook {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Hook, 0, len(s.hooks))
	for _, h := range s.hooks {
		out = append(out, *h)
	}
	return out
}

// ForEvent returns all enabled hooks matching an event type.
func (s *Store) ForEvent(event EventType) []Hook {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Hook
	for _, h := range s.hooks {
		if h.Enabled && h.Event == event {
			out = append(out, *h)
		}
	}
	return out
}

// Emit fires hooks registered for event (fire-and-forget).
func (s *Store) Emit(event EventType, payload map[string]any) {
	hooks := s.ForEvent(event)
	for _, h := range hooks {
		go s.dispatch(h, payload)
	}
}

func (s *Store) dispatch(h Hook, payload map[string]any) {
	switch h.Type {
	case HookTypeWebhook:
		target := strings.TrimSpace(h.Target)
		if target == "" {
			return
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(raw))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.client.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[hook:%s] webhook error: %v\n", h.Name, err)
			return
		}
		resp.Body.Close()
	default:
		fmt.Fprintf(os.Stderr, "[hook:%s] event=%s fired\n", h.Name, string(h.Event))
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
	var hooks []Hook
	if err := json.Unmarshal(raw, &hooks); err != nil {
		return nil
	}
	for i := range hooks {
		h := hooks[i]
		s.hooks[h.ID] = &h
	}
	return nil
}

func (s *Store) saveLocked() error {
	out := make([]Hook, 0, len(s.hooks))
	for _, h := range s.hooks {
		out = append(out, *h)
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFile(s.path, raw, 0o644)
}

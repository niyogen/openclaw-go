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
	EventAgentRunStarted  EventType = "agent.run.started"
	EventAgentRunComplete EventType = "agent.run.complete"
	EventToolInvoked      EventType = "tool.invoked"
	// Lifecycle: fired exactly once per gateway process. Useful for warmup
	// hooks (preload caches, ping a deadman, etc.) and for clean teardown
	// (flush buffers, notify monitoring).
	EventGatewayStarted  EventType = "gateway.started"
	EventGatewayStopping EventType = "gateway.stopping"
	// Approval lifecycle: fired when the executor enqueues a tool-call
	// approval request. Lets external systems surface the request via push,
	// Slack DM, etc. without having to long-poll `approvals.list`.
	EventApprovalRequested EventType = "approval.requested"
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

// maxConcurrentDispatches limits how many webhook goroutines may run at once
// to prevent burst events from exhausting goroutine/connection resources.
const maxConcurrentDispatches = 32

// EventListener is the signature for side-channel subscribers attached
// via AddListener. Listeners fire on every Emit alongside persistent
// Hook records but live in-memory only — used by code paths (channel
// plugins, push notifications, tool plugins, …) that need event
// notifications without polluting the operator-visible hook list. Each
// listener runs in its own goroutine, fire-and-forget. Listeners are
// expected to be cheap and quick; long-running work must be deferred
// to a goroutine inside the listener.
type EventListener func(event EventType, payload map[string]any)

// Store holds hooks and provides event dispatch.
type Store struct {
	mu        sync.Mutex
	hooks     map[string]*Hook
	path      string
	client    *http.Client
	dispSem   chan struct{} // semaphore bounding concurrent dispatches
	listeners []EventListener
}

// New opens (or creates) a hook store backed by path.
func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	sem := make(chan struct{}, maxConcurrentDispatches)
	for range maxConcurrentDispatches {
		sem <- struct{}{}
	}
	s := &Store{
		path:    path,
		hooks:   map[string]*Hook{},
		client:  &http.Client{Timeout: 10 * time.Second},
		dispSem: sem,
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

// AddListener registers a non-persistent EventListener that fires on
// every Emit alongside the persistent Hook records. Listeners run in
// their own goroutines, fire-and-forget. Use this for plugin-hook
// fan-out, push notifications, and other ephemeral subscribers that
// should NOT appear in the operator's persisted hook list.
//
// Safe to call after the store is constructed; listeners are read
// under the same lock that protects the hooks map.
func (s *Store) AddListener(l EventListener) {
	if l == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, l)
}

// Emit fires hooks registered for event (fire-and-forget, bounded concurrency).
// Listeners (added via AddListener) fan out in parallel; each runs in
// its own goroutine, NOT subject to the dispSem semaphore (callers are
// expected to be cheap — plugin hook listeners use their own HTTP
// client timeouts).
func (s *Store) Emit(event EventType, payload map[string]any) {
	hooks := s.ForEvent(event)
	// Snapshot listeners under the lock so the slice isn't mutated mid-iteration.
	s.mu.Lock()
	listeners := make([]EventListener, len(s.listeners))
	copy(listeners, s.listeners)
	s.mu.Unlock()

	for _, h := range hooks {
		// Go 1.22 made range-loop variables per-iteration, so the goroutine
		// below captures this iteration's `h` safely without the prior
		// `h := h` shadow idiom.
		select {
		case <-s.dispSem:
			go func() {
				defer func() { s.dispSem <- struct{}{} }()
				s.dispatch(h, payload)
			}()
		default:
			// Semaphore full — drop this dispatch rather than blocking or spawning.
		}
	}
	for _, l := range listeners {
		go l(event, payload)
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
		return fmt.Errorf("hookstore: corrupt data file %s: %w", s.path, err)
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
	return fileutil.WriteFile(s.path, raw, 0o600)
}

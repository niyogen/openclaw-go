package sessions

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Message struct {
	Role      Role      `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
}

type Session struct {
	ID        string    `json:"id"`
	Channel   string    `json:"channel"`
	Target    string    `json:"target"`
	Messages  []Message `json:"messages"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Store struct {
	path     string
	mu       sync.Mutex
	sessions map[string]*Session
}

func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		path:     path,
		sessions: map[string]*Session{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) UpsertSession(id, channel, target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[id]
	if !ok {
		s.sessions[id] = &Session{
			ID:        id,
			Channel:   channel,
			Target:    target,
			UpdatedAt: time.Now().UTC(),
		}
	}
	return s.saveLocked()
}

func (s *Store) AppendMessage(sessionID string, msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.sessions[sessionID]
	if !ok {
		return errors.New("session does not exist")
	}
	current.Messages = append(current.Messages, msg)
	current.UpdatedAt = time.Now().UTC()
	return s.saveLocked()
}

func (s *Store) List() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		copySess := *sess
		out = append(out, copySess)
	}
	return out
}

func (s *Store) Get(sessionID string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.sessions[sessionID]
	if !ok {
		return Session{}, false
	}
	copySess := *item
	return copySess, true
}

// Kill removes all messages from a session but keeps the session record.
func (s *Store) Kill(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	sess.Messages = nil
	sess.UpdatedAt = time.Now().UTC()
	return s.saveLocked()
}

// History returns the message slice for a session in transcript order.
func (s *Store) History(sessionID string) ([]Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil, false
	}
	out := make([]Message, len(sess.Messages))
	copy(out, sess.Messages)
	return out, true
}

// Patch applies a series of message replacements to a session.
// Each item in the patch must contain a non-negative index and new content.
func (s *Store) Patch(sessionID string, patches []MessagePatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	for _, p := range patches {
		if p.Index < 0 || p.Index >= len(sess.Messages) {
			return errors.New("patch index out of range")
		}
		sess.Messages[p.Index].Content = p.Content
	}
	sess.UpdatedAt = time.Now().UTC()
	return s.saveLocked()
}

// MessagePatch describes a single in-place message update.
type MessagePatch struct {
	Index   int    `json:"index"`
	Content string `json:"content"`
}

// Delete removes a session by id. Returns false if it did not exist.
func (s *Store) Delete(sessionID string) (deleted bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sessionID]; !ok {
		return false, nil
	}
	delete(s.sessions, sessionID)
	return true, s.saveLocked()
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
	var payload []Session
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	for i := range payload {
		item := payload[i]
		s.sessions[item.ID] = &item
	}
	return nil
}

func (s *Store) saveLocked() error {
	payload := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		payload = append(payload, *sess)
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0o644)
}

package sessions

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"openclaw-go/internal/fileutil"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
	RoleSystem    Role = "system"
)

// MessageType indicates the kind of content in a message.
type MessageType string

const (
	MessageTypeText       MessageType = "text"
	MessageTypeToolCall   MessageType = "tool_call"
	MessageTypeToolResult MessageType = "tool_result"
	MessageTypeImage      MessageType = "image"
	MessageTypeFile       MessageType = "file"
)

// Attachment is an image, file, or other binary payload.
type Attachment struct {
	Type     string `json:"type"` // "image", "file", "audio"
	URL      string `json:"url,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Name     string `json:"name,omitempty"`
}

// ToolCallData holds the function name and arguments for a tool_call message.
type ToolCallData struct {
	ToolCallID string         `json:"toolCallId"`
	Name       string         `json:"name"`
	Arguments  map[string]any `json:"arguments,omitempty"`
	Result     any            `json:"result,omitempty"`
}

type Message struct {
	Role        Role          `json:"role"`
	Type        MessageType   `json:"type,omitempty"`
	Content     string        `json:"content"`
	ToolCall    *ToolCallData `json:"toolCall,omitempty"`
	Attachments []Attachment  `json:"attachments,omitempty"`
	CreatedAt   time.Time     `json:"createdAt"`
}

type Session struct {
	ID        string    `json:"id"`
	Channel   string    `json:"channel"`
	Target    string    `json:"target"`
	Provider  string    `json:"provider,omitempty"` // per-session model provider override
	Model     string    `json:"model,omitempty"`    // per-session model override
	Messages  []Message `json:"messages"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Store struct {
	path        string
	mu          sync.Mutex
	sessions    map[string]*Session
	maxMessages int
	// Memory compaction: when memoryInlineTrim is true, after each append we trim
	// the oldest messages if count exceeds memoryCompactAfter.
	memoryCompactAfter int
	memoryInlineTrim   bool
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
	if existing, ok := s.sessions[id]; ok {
		// Update routing metadata so that channel switches (e.g. Telegram → CLI)
		// are reflected in subsequent lookups.
		if channel != "" {
			existing.Channel = channel
		}
		if target != "" {
			existing.Target = target
		}
	} else {
		s.sessions[id] = &Session{
			ID:        id,
			Channel:   channel,
			Target:    target,
			UpdatedAt: time.Now().UTC(),
		}
	}
	return s.saveLocked()
}

// SetMaxMessages configures the per-session message cap. When n > 0 and a
// session exceeds n messages after an append, the oldest messages are trimmed
// so that only the most recent n remain. Set to 0 to disable the cap.
func (s *Store) SetMaxMessages(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxMessages = n
}

// SetMemoryCompaction configures sliding-window trimming in AppendMessage / SetMessages.
// When inlineTrim is true and compactAfter > 0, persisted messages are trimmed from the
// front whenever the count would exceed compactAfter (no LLM summarization).
func (s *Store) SetMemoryCompaction(compactAfter int, inlineTrim bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memoryCompactAfter = compactAfter
	s.memoryInlineTrim = inlineTrim
}

func (s *Store) AppendMessage(sessionID string, msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.sessions[sessionID]
	if !ok {
		return errors.New("session does not exist")
	}
	current.Messages = append(current.Messages, msg)
	if s.maxMessages > 0 && len(current.Messages) > s.maxMessages {
		excess := len(current.Messages) - s.maxMessages
		current.Messages = current.Messages[excess:]
	}
	if s.memoryInlineTrim && s.memoryCompactAfter > 0 && len(current.Messages) > s.memoryCompactAfter {
		excess := len(current.Messages) - s.memoryCompactAfter
		current.Messages = current.Messages[excess:]
	}
	current.UpdatedAt = time.Now().UTC()
	return s.saveLocked()
}

// SetSessionModel sets the provider and model override for a session.
func (s *Store) SetSessionModel(id, provider, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return errors.New("session not found")
	}
	sess.Provider = strings.TrimSpace(provider)
	sess.Model = strings.TrimSpace(model)
	sess.UpdatedAt = time.Now().UTC()
	return s.saveLocked()
}

// Count returns the number of sessions without loading message payloads.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *Store) List() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, deepCopySession(sess))
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
	return deepCopySession(item), true
}

// deepCopyMessages returns an independently allocated copy of a message slice
// including nested Attachments and ToolCall pointers.
// Always returns a non-nil slice so callers serialise it as [] not null.
func deepCopyMessages(msgs []Message) []Message {
	out := make([]Message, len(msgs))
	if len(msgs) == 0 {
		return out
	}
	for i, m := range msgs {
		cm := m
		if len(m.Attachments) > 0 {
			cm.Attachments = make([]Attachment, len(m.Attachments))
			copy(cm.Attachments, m.Attachments)
		}
		if m.ToolCall != nil {
			tc := *m.ToolCall
			cm.ToolCall = &tc
		}
		out[i] = cm
	}
	return out
}

// deepCopySession returns a full copy of sess with every mutable slice and
// pointer independently allocated so callers cannot race against store
// mutations (AppendMessage, Patch, etc.).
func deepCopySession(sess *Session) Session {
	cp := *sess
	cp.Messages = deepCopyMessages(sess.Messages)
	return cp
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
	return deepCopyMessages(sess.Messages), true
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

// Create makes a new empty session (alternative to UpsertSession).
func (s *Store) Create(id, channel, target string) error {
	return s.UpsertSession(id, channel, target)
}

// Describe returns a human-readable summary of a session.
func (s *Store) Describe(sessionID string) (map[string]any, bool) {
	return s.Stats(sessionID)
}

// Preview returns the last N messages of a session (for quick display).
func (s *Store) Preview(sessionID string, n int) ([]Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil, false
	}
	msgs := sess.Messages
	if n > 0 && len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}
	return deepCopyMessages(msgs), true
}

// Abort marks a session as aborted (adds a system message).
func (s *Store) Abort(sessionID, reason string) error {
	return s.AppendMessage(sessionID, Message{
		Role:      RoleSystem,
		Type:      MessageTypeText,
		Content:   "[ABORTED] " + reason,
		CreatedAt: time.Now().UTC(),
	})
}

// Reset clears messages and resets UpdatedAt (like Kill but for restart flows).
func (s *Store) Reset(sessionID string) error {
	return s.Kill(sessionID)
}

// Cleanup removes sessions that have not been updated within maxAge.
func (s *Store) Cleanup(maxAge time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for id, sess := range s.sessions {
		if sess.UpdatedAt.Before(cutoff) {
			delete(s.sessions, id)
			removed++
		}
	}
	if removed > 0 {
		return removed, s.saveLocked()
	}
	return 0, nil
}

// Compact removes messages older than keepN from the start of the session.
// If keepN >= len(messages) nothing is removed.  Returns number removed.
func (s *Store) Compact(sessionID string, keepN int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return 0, errors.New("session not found")
	}
	total := len(sess.Messages)
	if keepN < 0 {
		keepN = 0
	}
	if keepN >= total {
		return 0, nil
	}
	removed := total - keepN
	sess.Messages = sess.Messages[removed:]
	sess.UpdatedAt = time.Now().UTC()
	return removed, s.saveLocked()
}

// SetMessages replaces the entire message list for a session (used after summarization).
func (s *Store) SetMessages(sessionID string, msgs []Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	sess.Messages = deepCopyMessages(msgs)
	if s.maxMessages > 0 && len(sess.Messages) > s.maxMessages {
		excess := len(sess.Messages) - s.maxMessages
		sess.Messages = sess.Messages[excess:]
	}
	if s.memoryInlineTrim && s.memoryCompactAfter > 0 && len(sess.Messages) > s.memoryCompactAfter {
		excess := len(sess.Messages) - s.memoryCompactAfter
		sess.Messages = sess.Messages[excess:]
	}
	sess.UpdatedAt = time.Now().UTC()
	return s.saveLocked()
}

// Stats returns summary statistics for a session.
func (s *Store) Stats(sessionID string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil, false
	}
	userCount, assistantCount := 0, 0
	for _, m := range sess.Messages {
		switch m.Role {
		case RoleUser:
			userCount++
		case RoleAssistant:
			assistantCount++
		}
	}
	return map[string]any{
		"sessionId":         sessionID,
		"messageCount":      len(sess.Messages),
		"userMessages":      userCount,
		"assistantMessages": assistantCount,
		"updatedAt":         sess.UpdatedAt,
	}, true
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
	return fileutil.WriteFile(s.path, raw, 0o644)
}

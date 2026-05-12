package sessions

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"openclaw-go/internal/fileutil"
)

// CompactionRecord captures a single explicit Compact() event so users can
// later inspect what was trimmed, restore the pre-compaction transcript onto
// the same session, or branch a new session from the snapshot.
//
// PreMessages is the full message list as it stood BEFORE the compaction —
// not just the removed prefix — so Restore can put the session back to its
// exact prior state without reconstructing tail messages.
type CompactionRecord struct {
	ID           string    `json:"id"`
	SessionID    string    `json:"sessionId"`
	At           time.Time `json:"at"`
	KeepN        int       `json:"keepN"`
	RemovedCount int       `json:"removedCount"`
	PreMessages  []Message `json:"preMessages"`
}

// compactionSeq disambiguates IDs created within the same nanosecond so two
// quick-succession Compact calls don't collide.
var compactionSeq atomic.Uint64

func newCompactionID() string {
	now := time.Now().UTC().Format("20060102-150405.999999999")
	n := compactionSeq.Add(1)
	return "cmp_" + now + "_" + strconv.FormatUint(n, 36)
}

func (s *Store) loadCompactions() error {
	raw, err := os.ReadFile(s.compactionsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	var list []*CompactionRecord
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	for _, rec := range list {
		if rec == nil || rec.ID == "" {
			continue
		}
		s.compactions[rec.ID] = rec
	}
	return nil
}

func (s *Store) saveCompactionsLocked() error {
	list := make([]*CompactionRecord, 0, len(s.compactions))
	for _, rec := range s.compactions {
		list = append(list, rec)
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFile(s.compactionsPath, raw, 0o600)
}

// recordCompactionLocked snapshots the pre-image of a session and persists
// the resulting CompactionRecord. Caller must hold s.mu.
func (s *Store) recordCompactionLocked(sessionID string, keepN, removed int, pre []Message) *CompactionRecord {
	rec := &CompactionRecord{
		ID:           newCompactionID(),
		SessionID:    sessionID,
		At:           time.Now().UTC(),
		KeepN:        keepN,
		RemovedCount: removed,
		PreMessages:  deepCopyMessages(pre),
	}
	s.compactions[rec.ID] = rec
	_ = s.saveCompactionsLocked()
	return rec
}

// CompactionList returns all compaction records for a session in chronological
// order (oldest first). Returns an empty slice if the session has none.
func (s *Store) CompactionList(sessionID string) []CompactionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []CompactionRecord
	for _, rec := range s.compactions {
		if rec.SessionID != sessionID {
			continue
		}
		cp := *rec
		cp.PreMessages = deepCopyMessages(rec.PreMessages)
		out = append(out, cp)
	}
	// Chronological order is stable and predictable for clients.
	sortCompactionsByAt(out)
	return out
}

// CompactionGet returns a single record by ID with its full PreMessages.
func (s *Store) CompactionGet(id string) (CompactionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.compactions[id]
	if !ok {
		return CompactionRecord{}, false
	}
	cp := *rec
	cp.PreMessages = deepCopyMessages(rec.PreMessages)
	return cp, true
}

// CompactionRestore replaces the session's current Messages with the snapshot
// captured at the time of the named compaction. The current (post-compaction)
// transcript is discarded; if the caller wants to keep a fork, use
// CompactionBranch instead.
func (s *Store) CompactionRestore(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.compactions[id]
	if !ok {
		return errors.New("compaction record not found")
	}
	sess, ok := s.sessions[rec.SessionID]
	if !ok {
		return errors.New("session not found")
	}
	sess.Messages = deepCopyMessages(rec.PreMessages)
	sess.UpdatedAt = time.Now().UTC()
	return s.saveLocked()
}

// CompactionBranch creates a new session whose messages are seeded with the
// compaction's PreMessages. The original session is left untouched. If
// newSessionID is empty, a timestamp-based ID is generated. Returns the
// freshly-created session.
func (s *Store) CompactionBranch(id, newSessionID string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.compactions[id]
	if !ok {
		return Session{}, errors.New("compaction record not found")
	}
	parent, ok := s.sessions[rec.SessionID]
	if !ok {
		return Session{}, errors.New("session not found")
	}
	if newSessionID == "" {
		newSessionID = "branch_" + newCompactionID()
	}
	if _, exists := s.sessions[newSessionID]; exists {
		return Session{}, errors.New("target session already exists")
	}
	branch := &Session{
		ID:        newSessionID,
		Channel:   parent.Channel,
		Target:    parent.Target,
		Provider:  parent.Provider,
		Model:     parent.Model,
		Messages:  deepCopyMessages(rec.PreMessages),
		UpdatedAt: time.Now().UTC(),
	}
	s.sessions[newSessionID] = branch
	if err := s.saveLocked(); err != nil {
		// Roll back the in-memory insert so the on-disk and in-memory views
		// stay consistent.
		delete(s.sessions, newSessionID)
		return Session{}, err
	}
	return deepCopySession(branch), nil
}

// sortCompactionsByAt orders records oldest-first by At, with ID as a stable
// tiebreaker (IDs embed timestamps, so this is consistent with At).
func sortCompactionsByAt(records []CompactionRecord) {
	if len(records) < 2 {
		return
	}
	for i := 1; i < len(records); i++ {
		j := i
		for j > 0 && (records[j-1].At.After(records[j].At) ||
			(records[j-1].At.Equal(records[j].At) && records[j-1].ID > records[j].ID)) {
			records[j-1], records[j] = records[j], records[j-1]
			j--
		}
	}
}

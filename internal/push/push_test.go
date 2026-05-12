package push

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// recordingSender records every Send call. Tests stage scenarios then
// assert what would have been pushed without hitting a real provider.
type recordingSender struct {
	mu       sync.Mutex
	sends    []sentMessage
	sendErr  error // if non-nil, every Send returns this
	sendErrs map[string]error
}

type sentMessage struct {
	SubID   string
	Payload []byte
}

func (r *recordingSender) Send(_ context.Context, sub Subscription, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, sentMessage{SubID: sub.ID, Payload: append([]byte(nil), payload...)})
	if r.sendErrs != nil {
		if err, ok := r.sendErrs[sub.ID]; ok {
			return err
		}
	}
	return r.sendErr
}

func (r *recordingSender) sent() []sentMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sentMessage, len(r.sends))
	copy(out, r.sends)
	return out
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	s, err := NewService(dir, "mailto:test@example.com")
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s.SetSender(&recordingSender{}) // default fake; tests can swap
	return s
}

func TestSubscribeRoundTripPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	s, err := NewService(dir, "mailto:test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	s.SetSender(&recordingSender{})

	sub, err := s.Subscribe("https://push.example.com/abc", "p256dh-key", "auth-key", "Alice's phone")
	if err != nil {
		t.Fatal(err)
	}
	if sub.ID == "" {
		t.Fatal("sub.ID empty")
	}
	if sub.Label != "Alice's phone" {
		t.Fatalf("label: %q", sub.Label)
	}

	// Re-open the Service in the same dir; subscription should reload.
	s2, err := NewService(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	list := s2.List()
	if len(list) != 1 || list[0].ID != sub.ID {
		t.Fatalf("expected 1 subscription with id %s after reload, got %+v", sub.ID, list)
	}
}

func TestSubscribeRejectsEmptyFields(t *testing.T) {
	s := newTestService(t)
	cases := []struct {
		ep, p, a string
	}{
		{"", "p", "a"},
		{"ep", "", "a"},
		{"ep", "p", ""},
	}
	for _, tc := range cases {
		if _, err := s.Subscribe(tc.ep, tc.p, tc.a, ""); err == nil {
			t.Fatalf("expected error for empty field; ep=%q p=%q a=%q", tc.ep, tc.p, tc.a)
		}
	}
}

func TestUnsubscribeRemovesById(t *testing.T) {
	s := newTestService(t)
	sub, _ := s.Subscribe("e1", "p1", "a1", "")
	if err := s.Unsubscribe(sub.ID); err != nil {
		t.Fatal(err)
	}
	if list := s.List(); len(list) != 0 {
		t.Fatalf("expected empty after unsubscribe; got %d", len(list))
	}
}

func TestUnsubscribeIsIdempotent(t *testing.T) {
	s := newTestService(t)
	if err := s.Unsubscribe("never-existed"); err != nil {
		t.Fatalf("unsubscribe of unknown id should not error; got %v", err)
	}
}

func TestSendAllFansOutToEverySubscription(t *testing.T) {
	s := newTestService(t)
	rec := &recordingSender{}
	s.SetSender(rec)

	for i := range 3 {
		_, _ = s.Subscribe(
			"https://push.example.com/"+string(rune('a'+i)),
			"p256dh", "auth", "")
	}
	if err := s.SendAll(context.Background(), []byte(`{"hi":"there"}`)); err != nil {
		t.Fatalf("SendAll: %v", err)
	}
	if got := len(rec.sent()); got != 3 {
		t.Fatalf("expected 3 fan-out sends, got %d", got)
	}
}

func TestSendAllAggregatesFirstError(t *testing.T) {
	s := newTestService(t)
	sub1, _ := s.Subscribe("e1", "p", "a", "")
	sub2, _ := s.Subscribe("e2", "p", "a", "")
	sub3, _ := s.Subscribe("e3", "p", "a", "")
	// Make sub2's send fail; sub1 and sub3 succeed.
	rec := &recordingSender{
		sendErrs: map[string]error{
			sub2.ID: errors.New("provider unreachable"),
		},
	}
	s.SetSender(rec)
	err := s.SendAll(context.Background(), []byte("payload"))
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	// All 3 should have been attempted regardless.
	if len(rec.sent()) != 3 {
		t.Fatalf("all 3 subs should be attempted; got %d sends", len(rec.sent()))
	}
	_ = sub1
	_ = sub3
}

func TestSendOneReturnsErrorForUnknownID(t *testing.T) {
	s := newTestService(t)
	if err := s.SendOne(context.Background(), "bogus", []byte("x")); err == nil {
		t.Fatal("expected error for unknown id")
	}
}

func TestPublicKeyIsStableAcrossReload(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewService(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	key1 := s1.PublicKey()
	if key1 == "" {
		t.Fatal("public key should be non-empty after generation")
	}

	s2, err := NewService(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.PublicKey(); got != key1 {
		t.Fatalf("public key drifted across reload: %q vs %q", key1, got)
	}
}

func TestKeysFilePersistedAtMode0600(t *testing.T) {
	// On Windows the mode bit reflects the read-only flag, not POSIX
	// permissions — skip there. Linux/macOS test runs verify 0o600.
	if !hasPosixPermissions() {
		t.Skip("file mode bits not meaningful on this OS")
	}
	dir := t.TempDir()
	_, err := NewService(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "push-keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected 0o600, got %o", got)
	}
}

// hasPosixPermissions reports whether file mode bits are meaningful.
// Returns true on Unix-like systems, false on Windows.
func hasPosixPermissions() bool {
	// A simple probe: create a file with 0o600 and check the result.
	// On Windows the bits don't round-trip; on Unix they do.
	tmp, err := os.CreateTemp("", "perm-probe-")
	if err != nil {
		return false
	}
	defer os.Remove(tmp.Name())
	tmp.Close()
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return false
	}
	info, err := os.Stat(tmp.Name())
	if err != nil {
		return false
	}
	return info.Mode().Perm() == 0o600
}

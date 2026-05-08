package hookstore

import (
	"path/filepath"
	"testing"
)

func TestHookStoreAddRemoveList(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	hook := Hook{
		ID:      "h1",
		Name:    "test hook",
		Event:   EventMessageReceived,
		Type:    HookTypeLog,
		Enabled: true,
	}
	if err := s.Add(hook); err != nil {
		t.Fatal(err)
	}
	all := s.List()
	if len(all) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(all))
	}
	forEvent := s.ForEvent(EventMessageReceived)
	if len(forEvent) != 1 {
		t.Fatalf("expected 1 hook for event, got %d", len(forEvent))
	}
	forOther := s.ForEvent(EventToolInvoked)
	if len(forOther) != 0 {
		t.Fatalf("expected 0 hooks for other event, got %d", len(forOther))
	}
	deleted, err := s.Remove("h1")
	if err != nil || !deleted {
		t.Fatalf("Remove: deleted=%v err=%v", deleted, err)
	}
	if len(s.List()) != 0 {
		t.Fatal("expected 0 hooks after remove")
	}
}

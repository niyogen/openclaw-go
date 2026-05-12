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

// TestHookStoreLifecycleEventTypes pins the new event-type constants added in
// P0.6 so a careless rename in hookstore.go can't silently disconnect every
// subscriber that registered under the old name.
func TestHookStoreLifecycleEventTypes(t *testing.T) {
	cases := map[EventType]string{
		EventGatewayStarted:    "gateway.started",
		EventGatewayStopping:   "gateway.stopping",
		EventAgentRunStarted:   "agent.run.started",
		EventApprovalRequested: "approval.requested",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Fatalf("event constant drifted: got %q want %q", got, want)
		}
	}
}

// TestHookStoreLifecycleRoutesByEvent confirms ForEvent correctly partitions
// hooks across the new lifecycle events without leakage.
func TestHookStoreLifecycleRoutesByEvent(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	events := []EventType{
		EventGatewayStarted,
		EventGatewayStopping,
		EventAgentRunStarted,
		EventApprovalRequested,
	}
	for i, ev := range events {
		if err := s.Add(Hook{
			ID:      string(ev),
			Name:    string(ev),
			Event:   ev,
			Type:    HookTypeLog,
			Enabled: true,
		}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	for _, ev := range events {
		got := s.ForEvent(ev)
		if len(got) != 1 {
			t.Fatalf("ForEvent(%s): got %d hooks want 1", ev, len(got))
		}
		if got[0].ID != string(ev) {
			t.Fatalf("ForEvent(%s) returned wrong hook %q", ev, got[0].ID)
		}
	}
}

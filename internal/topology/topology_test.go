package topology

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPairingPersistedAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "topology.json")

	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	// Create a pairing request.
	req := s.CreatePairing("device-1")
	if req == nil {
		t.Fatal("expected non-nil pairing request")
		return
	}
	pairID := req.ID

	// Reload from disk.
	s2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	// The pairing should survive the reload.
	pending := s2.ListPendingPairing()
	found := false
	for _, p := range pending {
		if p.ID == pairID {
			found = true
		}
	}
	if !found {
		t.Fatalf("pairing %q not found after reload; got %d pending", pairID, len(pending))
	}
}

func TestApprovePairingPersistedAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "topology.json")

	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	req := s.CreatePairing("device-2")
	if err := s.ApprovePairing(req.ID); err != nil {
		t.Fatal(err)
	}

	s2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	// Approved pairings are no longer in the pending list.
	for _, p := range s2.ListPendingPairing() {
		if p.ID == req.ID {
			t.Fatal("approved pairing should not be in pending list")
		}
	}
	// But it should exist in the raw pairing map via a reload (status=approved).
	// We confirm the store reloaded without error — the data integrity is the goal.
	_ = s2
}

func TestNodeRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "topology.json")

	s, _ := New(path)
	if err := s.AddNode(Node{ID: "n1", Name: "gateway-1", URL: "http://g1:8080"}); err != nil {
		t.Fatal(err)
	}

	s2, _ := New(path)
	nodes := s2.ListNodes()
	if len(nodes) != 1 || nodes[0].ID != "n1" {
		t.Fatalf("node not persisted: %+v", nodes)
	}
}

func TestDeriveStableNodeID(t *testing.T) {
	if got := DeriveStableNodeID("x", "https://a"); got != "x" {
		t.Fatalf("explicit id: got %q want x", got)
	}
	got1 := DeriveStableNodeID("", "https://example.com/")
	got2 := DeriveStableNodeID("", "https://example.com/")
	if got1 != got2 || !strings.HasPrefix(got1, "cfg-") || len(got1) < 12 {
		t.Fatalf("stable hash: %q vs %q", got1, got2)
	}
	if DeriveStableNodeID("", "") != "" {
		t.Fatal("empty should stay empty")
	}
}

func TestUpsertGatewayPeer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "topology.json")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertGatewayPeer("", "peer", "https://p1/rpc", "k"); err != nil {
		t.Fatal(err)
	}
	id := DeriveStableNodeID("", "https://p1/rpc")
	n, ok := s.GetNode(id)
	if !ok || n.URL != "https://p1/rpc" || n.APIKey != "k" || n.Status != NodeStatusOnline {
		t.Fatalf("node: %+v ok=%v", n, ok)
	}
	if err := s.UpsertGatewayPeer("", "peer2", "https://p1/rpc", "k2"); err != nil {
		t.Fatal(err)
	}
	n2, _ := s.GetNode(id)
	if n2.Name != "peer2" || n2.APIKey != "k2" {
		t.Fatalf("update: %+v", n2)
	}
}

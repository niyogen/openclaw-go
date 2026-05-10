package gateway

import (
	"testing"

	"openclaw-go/internal/config"
	"openclaw-go/internal/topology"
)

func TestSyncNodesFromConfig(t *testing.T) {
	s := buildTestServer(t, "")
	url := "https://cfg-peer.example/rpc"
	derived := topology.DeriveStableNodeID("", url)

	if err := s.SyncNodesFromConfig([]config.NodeConfig{
		{Enabled: true, Name: "P", URL: url, APIKey: "sek"},
	}); err != nil {
		t.Fatal(err)
	}
	n, ok := s.topo.GetNode(derived)
	if !ok || n.APIKey != "sek" || n.Name != "P" {
		t.Fatalf("upsert: %+v ok=%v", n, ok)
	}

	if err := s.SyncNodesFromConfig([]config.NodeConfig{
		{Enabled: false, URL: url},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.topo.GetNode(derived); ok {
		t.Fatal("expected removed")
	}

	if err := s.SyncNodesFromConfig([]config.NodeConfig{
		{Enabled: true, ID: "n99", URL: "https://x", APIKey: "a"},
	}); err != nil {
		t.Fatal(err)
	}
	nn, ok := s.topo.GetNode("n99")
	if !ok || nn.URL != "https://x" {
		t.Fatalf("explicit id: %+v", nn)
	}
}

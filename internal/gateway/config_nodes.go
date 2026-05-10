package gateway

import (
	"strings"

	"openclaw-go/internal/config"
	"openclaw-go/internal/topology"
)

// SyncNodesFromConfig applies openclaw.json "nodes" to the topology store so
// node.invoke can reach configured peers. Enabled entries are upserted; disabled
// entries remove the matching node (same id rules as UpsertGatewayPeer).
func (s *Server) SyncNodesFromConfig(nodes []config.NodeConfig) error {
	if s == nil || s.topo == nil {
		return nil
	}
	for _, n := range nodes {
		resolved := topology.DeriveStableNodeID(n.ID, n.URL)
		if !n.Enabled {
			if resolved == "" {
				continue
			}
			if _, err := s.topo.RemoveNode(resolved); err != nil {
				return err
			}
			continue
		}
		if strings.TrimSpace(n.URL) == "" {
			continue
		}
		if err := s.topo.UpsertGatewayPeer(n.ID, n.Name, n.URL, n.APIKey); err != nil {
			return err
		}
	}
	return nil
}

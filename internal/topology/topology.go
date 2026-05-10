// Package topology manages distributed gateway nodes and paired devices.
package topology

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"openclaw-go/internal/fileutil"
)

// NodeStatus describes the state of a remote gateway node.
type NodeStatus string

const (
	NodeStatusOnline  NodeStatus = "online"
	NodeStatusOffline NodeStatus = "offline"
	NodeStatusPending NodeStatus = "pending"
)

// Node represents a remote openclaw-go gateway node.
type Node struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	URL       string     `json:"url"`
	APIKey    string     `json:"apiKey,omitempty"`
	Status    NodeStatus `json:"status"`
	LastSeen  *time.Time `json:"lastSeen,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// Device represents a paired client device.
type Device struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Token       string     `json:"token,omitempty"`
	PairingCode string     `json:"pairingCode,omitempty"`
	Status      string     `json:"status"` // "paired", "pending", "revoked"
	CreatedAt   time.Time  `json:"createdAt"`
	PairedAt    *time.Time `json:"pairedAt,omitempty"`
}

// PairingRequest is created when a device initiates pairing.
type PairingRequest struct {
	ID        string    `json:"id"`
	DeviceID  string    `json:"deviceId"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
	Status    string    `json:"status"` // "pending", "approved", "rejected"
}

// Store manages nodes, devices and pairing state.
type Store struct {
	mu      sync.Mutex
	nodes   map[string]*Node
	devices map[string]*Device
	pairing map[string]*PairingRequest
	path    string
}

func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		path:    path,
		nodes:   map[string]*Node{},
		devices: map[string]*Device{},
		pairing: map[string]*PairingRequest{},
	}
	_ = s.load()
	return s, nil
}

// ── Nodes ──────────────────────────────────────────────────────────────────

func (s *Store) AddNode(n Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n.ID == "" {
		n.ID = fmt.Sprintf("node-%d", time.Now().UnixNano())
	}
	n.CreatedAt = time.Now().UTC()
	n.Status = NodeStatusPending
	s.nodes[n.ID] = &n
	return s.save()
}

func (s *Store) ListNodes() []Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, *n)
	}
	return out
}

func (s *Store) GetNode(id string) (Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	if !ok {
		return Node{}, false
	}
	return *n, true
}

func (s *Store) RemoveNode(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[id]; !ok {
		return false, nil
	}
	delete(s.nodes, id)
	return true, s.save()
}

func (s *Store) UpdateNodeStatus(id string, status NodeStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	if !ok {
		return errors.New("node not found")
	}
	n.Status = status
	now := time.Now().UTC()
	n.LastSeen = &now
	return s.save()
}

// ── Devices ───────────────────────────────────────────────────────────────

func (s *Store) AddDevice(d Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.ID == "" {
		d.ID = fmt.Sprintf("dev-%d", time.Now().UnixNano())
	}
	d.CreatedAt = time.Now().UTC()
	d.Status = "pending"
	s.devices[d.ID] = &d
	return s.save()
}

func (s *Store) ListDevices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, *d)
	}
	return out
}

func (s *Store) ApproveDevice(id string) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return Device{}, errors.New("device not found")
	}
	d.Status = "paired"
	now := time.Now().UTC()
	d.PairedAt = &now
	d.Token = fmt.Sprintf("dtok-%d", now.UnixNano())
	if err := s.save(); err != nil {
		return Device{}, err
	}
	return *d, nil
}

func (s *Store) RevokeDevice(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return errors.New("device not found")
	}
	d.Status = "revoked"
	d.Token = ""
	return s.save()
}

// ── Pairing ───────────────────────────────────────────────────────────────

func (s *Store) CreatePairing(deviceID string) *PairingRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	code := fmt.Sprintf("%06d", time.Now().UnixNano()%1000000)
	req := &PairingRequest{
		ID:        fmt.Sprintf("pair-%d", time.Now().UnixNano()),
		DeviceID:  deviceID,
		Code:      code,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
		Status:    "pending",
	}
	s.pairing[req.ID] = req
	_ = s.save()
	return req
}

func (s *Store) ListPendingPairing() []PairingRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PairingRequest
	now := time.Now()
	for _, p := range s.pairing {
		if p.Status == "pending" && p.ExpiresAt.After(now) {
			out = append(out, *p)
		}
	}
	return out
}

// RejectPairing marks a pairing request as rejected and persists the change.
func (s *Store) RejectPairing(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pairing[id]
	if !ok {
		return errors.New("pairing request not found")
	}
	p.Status = "rejected"
	return s.save()
}

func (s *Store) ApprovePairing(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pairing[id]
	if !ok {
		return errors.New("pairing request not found")
	}
	p.Status = "approved"
	return s.save()
}

// ── Persistence ───────────────────────────────────────────────────────────

type persistedState struct {
	Nodes   []*Node          `json:"nodes"`
	Devices []*Device        `json:"devices"`
	Pairing []*PairingRequest `json:"pairing,omitempty"`
}

func (s *Store) save() error {
	state := persistedState{
		Nodes:   make([]*Node, 0, len(s.nodes)),
		Devices: make([]*Device, 0, len(s.devices)),
		Pairing: make([]*PairingRequest, 0, len(s.pairing)),
	}
	for _, n := range s.nodes {
		state.Nodes = append(state.Nodes, n)
	}
	for _, d := range s.devices {
		state.Devices = append(state.Devices, d)
	}
	for _, p := range s.pairing {
		state.Pairing = append(state.Pairing, p)
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFile(s.path, raw, 0o644)
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	var state persistedState
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("topology: corrupt state file %s: %w", s.path, err)
	}
	for _, n := range state.Nodes {
		s.nodes[n.ID] = n
	}
	for _, d := range state.Devices {
		s.devices[d.ID] = d
	}
	for _, p := range state.Pairing {
		s.pairing[p.ID] = p
	}
	return nil
}

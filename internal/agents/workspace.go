// Package agents — multi-agent workspace management.
// Agents are named configurations that can be listed, created, updated, and
// assigned to conversations. Each agent can have its own provider, system
// prompt, tools policy, and memory settings.
package agents

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

// AgentProfile is a named agent configuration.
type AgentProfile struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	Instructions string    `json:"instructions"` // system prompt
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	MaxTurns     int       `json:"maxTurns"`
	AllowedTools []string  `json:"allowedTools,omitempty"`
	DeniedTools  []string  `json:"deniedTools,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Artifact is a file/data asset produced or consumed by an agent.
type Artifact struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"` // "text", "json", "image", "file"
	Content   string    `json:"content,omitempty"`
	URL       string    `json:"url,omitempty"`
	AgentID   string    `json:"agentId,omitempty"`
	SessionID string    `json:"sessionId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Workspace manages agent profiles and artifacts.
type Workspace struct {
	mu        sync.Mutex
	agents    map[string]*AgentProfile
	artifacts map[string]*Artifact
	path      string
}

func NewWorkspace(path string) (*Workspace, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	w := &Workspace{
		path:      path,
		agents:    map[string]*AgentProfile{},
		artifacts: map[string]*Artifact{},
	}
	if err := w.load(); err != nil {
		return nil, err
	}
	return w, nil
}

// ── Agent profiles ─────────────────────────────────────────────────────────

func (w *Workspace) Create(profile AgentProfile) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if profile.ID == "" {
		profile.ID = fmt.Sprintf("agent-%d", time.Now().UnixNano())
	}
	now := time.Now().UTC()
	profile.CreatedAt = now
	profile.UpdatedAt = now
	w.agents[profile.ID] = &profile
	return w.save()
}

func (w *Workspace) Update(profile AgentProfile) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	existing, ok := w.agents[profile.ID]
	if !ok {
		return errors.New("agent not found: " + profile.ID)
	}
	profile.CreatedAt = existing.CreatedAt
	profile.UpdatedAt = time.Now().UTC()
	w.agents[profile.ID] = &profile
	return w.save()
}

func (w *Workspace) Delete(id string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.agents[id]; !ok {
		return false, nil
	}
	delete(w.agents, id)
	return true, w.save()
}

func (w *Workspace) Get(id string) (AgentProfile, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a, ok := w.agents[id]
	if !ok {
		return AgentProfile{}, false
	}
	return *a, true
}

func (w *Workspace) List() []AgentProfile {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]AgentProfile, 0, len(w.agents))
	for _, a := range w.agents {
		out = append(out, *a)
	}
	return out
}

// ── Artifacts ──────────────────────────────────────────────────────────────

func (w *Workspace) AddArtifact(a Artifact) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if a.ID == "" {
		a.ID = fmt.Sprintf("art-%d", time.Now().UnixNano())
	}
	a.CreatedAt = time.Now().UTC()
	w.artifacts[a.ID] = &a
	return w.save()
}

func (w *Workspace) GetArtifact(id string) (Artifact, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a, ok := w.artifacts[id]
	if !ok {
		return Artifact{}, false
	}
	return *a, true
}

func (w *Workspace) ListArtifacts(agentID string) []Artifact {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Artifact, 0)
	for _, a := range w.artifacts {
		if agentID == "" || a.AgentID == agentID {
			out = append(out, *a)
		}
	}
	return out
}

func (w *Workspace) DeleteArtifact(id string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.artifacts[id]; !ok {
		return false, nil
	}
	delete(w.artifacts, id)
	return true, w.save()
}

// ── Persistence ────────────────────────────────────────────────────────────

type workspaceState struct {
	Agents    []*AgentProfile `json:"agents"`
	Artifacts []*Artifact     `json:"artifacts"`
}

func (w *Workspace) save() error {
	state := workspaceState{
		Agents:    make([]*AgentProfile, 0, len(w.agents)),
		Artifacts: make([]*Artifact, 0, len(w.artifacts)),
	}
	for _, a := range w.agents {
		state.Agents = append(state.Agents, a)
	}
	for _, a := range w.artifacts {
		state.Artifacts = append(state.Artifacts, a)
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFile(w.path, raw, 0o600)
}

func (w *Workspace) load() error {
	raw, err := os.ReadFile(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	var state workspaceState
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("workspace: corrupt data file %s: %w", w.path, err)
	}
	for _, a := range state.Agents {
		w.agents[a.ID] = a
	}
	for _, a := range state.Artifacts {
		w.artifacts[a.ID] = a
	}
	return nil
}

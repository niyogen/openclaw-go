// Package runtime implements the agent execution runtime:
// exec policies, approval flow, retry loop, and subagent delegation.
package runtime

import (
	"fmt"
	"strings"
)

// ExecPolicy controls what the agent is allowed to do for a session.
type ExecPolicy struct {
	// AllowedTools is the set of tool names the agent may call without approval.
	// An empty slice means all tools are allowed.
	AllowedTools []string `json:"allowedTools"`

	// DeniedTools is the set of tool names the agent may never call.
	DeniedTools []string `json:"deniedTools"`

	// RequireApproval lists tool names that must be approved before execution.
	RequireApproval []string `json:"requireApproval"`

	// MaxRetries controls how many times a failed tool call may be retried.
	MaxRetries int `json:"maxRetries"`

	// MaxTurns is the maximum number of agent turns before the run is halted.
	MaxTurns int `json:"maxTurns"`
}

// DefaultPolicy returns a permissive policy suitable for local/trusted use.
func DefaultPolicy() ExecPolicy {
	return ExecPolicy{
		MaxRetries: 2,
		MaxTurns:   20,
	}
}

func (p ExecPolicy) normalize() ExecPolicy {
	if p.MaxRetries <= 0 {
		p.MaxRetries = 2
	}
	if p.MaxTurns <= 0 {
		p.MaxTurns = 20
	}
	return p
}

type policyDecision int

const (
	policyAllow   policyDecision = iota
	policyDeny                   // hard deny, error immediately
	policyApprove                // route through approval queue
)

func (p ExecPolicy) evaluate(toolName string) policyDecision {
	name := strings.ToLower(strings.TrimSpace(toolName))

	for _, denied := range p.DeniedTools {
		if strings.ToLower(strings.TrimSpace(denied)) == name {
			return policyDeny
		}
	}
	for _, needsApproval := range p.RequireApproval {
		if strings.ToLower(strings.TrimSpace(needsApproval)) == name {
			return policyApprove
		}
	}
	if len(p.AllowedTools) == 0 {
		return policyAllow
	}
	for _, allowed := range p.AllowedTools {
		if strings.ToLower(strings.TrimSpace(allowed)) == name {
			return policyAllow
		}
	}
	return policyDeny
}

// ErrToolDenied is returned when a policy hard-denies a tool call.
type ErrToolDenied struct {
	Tool string
}

func (e *ErrToolDenied) Error() string {
	return fmt.Sprintf("tool %q is denied by exec policy", e.Tool)
}

// ErrTurnsExceeded is returned when the agent exhausts its turn budget.
type ErrTurnsExceeded struct {
	Max int
}

func (e *ErrTurnsExceeded) Error() string {
	return fmt.Sprintf("agent exceeded maximum turns (%d)", e.Max)
}

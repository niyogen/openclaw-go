package gateway

import (
	"errors"
	"sync"
	"time"
)

// nodeCircuitSettings controls when a peer is considered unhealthy for node.invoke.
type nodeCircuitSettings struct {
	Threshold int           // consecutive failed invokes to open the circuit (default 5)
	Cooldown  time.Duration // how long to reject calls while open (default 30s)
}

func defaultNodeCircuitSettings() nodeCircuitSettings {
	return nodeCircuitSettings{
		Threshold: 5,
		Cooldown:  30 * time.Second,
	}
}

var errNodeCircuitOpen = errors.New("node circuit open: peer temporarily unavailable")

const (
	cbClosed = iota
	cbOpen
	cbHalfOpen
)

type nodeCircuit struct {
	mu        sync.Mutex
	state     int
	failures  int
	openUntil time.Time
	cfg       nodeCircuitSettings
}

func newNodeCircuit(cfg nodeCircuitSettings) *nodeCircuit {
	return &nodeCircuit{cfg: cfg}
}

// before returns errNodeCircuitOpen when the circuit is open and cooldown has not elapsed.
func (c *nodeCircuit) before() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	switch c.state {
	case cbOpen:
		if now.Before(c.openUntil) {
			return errNodeCircuitOpen
		}
		c.state = cbHalfOpen
		return nil
	case cbHalfOpen, cbClosed:
		return nil
	default:
		return nil
	}
}

func (c *nodeCircuit) recordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures = 0
	c.state = cbClosed
	c.openUntil = time.Time{}
}

func (c *nodeCircuit) recordFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case cbHalfOpen:
		c.state = cbOpen
		c.openUntil = time.Now().Add(c.cfg.Cooldown)
	case cbClosed:
		c.failures++
		th := c.cfg.Threshold
		if th < 1 {
			th = 1
		}
		if c.failures >= th {
			c.state = cbOpen
			c.openUntil = time.Now().Add(c.cfg.Cooldown)
			c.failures = 0
		}
	case cbOpen:
		// no-op
	}
}

// isBlocking returns true if new calls are rejected without hitting the peer.
func (c *nodeCircuit) isBlocking(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == cbOpen && now.Before(c.openUntil)
}

type nodeBreakerRegistry struct {
	mu       sync.Mutex
	breakers map[string]*nodeCircuit
	cfg      nodeCircuitSettings
}

func newNodeBreakerRegistry(cfg nodeCircuitSettings) *nodeBreakerRegistry {
	return &nodeBreakerRegistry{
		breakers: make(map[string]*nodeCircuit),
		cfg:      cfg,
	}
}

func (r *nodeBreakerRegistry) get(nodeID string) *nodeCircuit {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.breakers[nodeID]
	if !ok {
		c = newNodeCircuit(r.cfg)
		r.breakers[nodeID] = c
	}
	return c
}

func (r *nodeBreakerRegistry) snapshotStates(now time.Time) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]bool, len(r.breakers))
	for id, c := range r.breakers {
		out[id] = c.isBlocking(now)
	}
	return out
}

// shouldTripNodeCircuit returns false for client/param errors that are not peer health.
func shouldTripNodeCircuit(rpcErr *rpcError) bool {
	if rpcErr == nil {
		return false
	}
	switch rpcErr.Code {
	case -32602, -32603:
		return false
	default:
		return true
	}
}

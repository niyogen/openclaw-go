package gateway

import (
	"sync"
)

// EventType labels what happened.
type EventType string

const (
	EventSessionMessage EventType = "session.message"  // new message appended (user or assistant)
	EventSessionCreated EventType = "session.created"  // new session opened
	EventSessionKilled  EventType = "session.killed"   // messages cleared
	EventSessionDeleted EventType = "session.deleted"  // session removed
	EventAgentReply     EventType = "agent.reply"      // assistant reply ready
	EventApproval       EventType = "approval.pending" // tool call awaiting approval
)

// GatewayEvent is the payload broadcast to subscribers.
type GatewayEvent struct {
	Type      EventType `json:"type"`
	SessionID string    `json:"sessionId,omitempty"`
	Data      any       `json:"data,omitempty"`
}

// subscriber holds a channel for one WS client.
type subscriber struct {
	ch        chan GatewayEvent
	sessionID string // empty = all sessions
}

// EventBus fans out GatewayEvents to all registered WS subscribers.
type EventBus struct {
	mu   sync.RWMutex
	subs map[uint64]*subscriber
	seq  uint64
}

func NewEventBus() *EventBus {
	return &EventBus{subs: map[uint64]*subscriber{}}
}

// Subscribe registers a channel to receive events.
// Pass sessionID="" to receive all events; otherwise only that session's events.
// Returns an unsubscribe function.
func (b *EventBus) Subscribe(sessionID string) (<-chan GatewayEvent, func()) {
	ch := make(chan GatewayEvent, 32)
	b.mu.Lock()
	b.seq++
	id := b.seq
	b.subs[id] = &subscriber{ch: ch, sessionID: sessionID}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
		// Drain so sender goroutines don't block.
		for len(ch) > 0 {
			<-ch
		}
	}
}

// Publish delivers an event to all matching subscribers (non-blocking).
func (b *EventBus) Publish(ev GatewayEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.subs {
		if s.sessionID != "" && s.sessionID != ev.SessionID {
			continue
		}
		select {
		case s.ch <- ev:
		default: // drop if subscriber is too slow
		}
	}
}

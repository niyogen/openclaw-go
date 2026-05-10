package gateway

import "sync"

// EventType labels what happened.
type EventType string

const (
	// Session events
	EventSessionMessage  EventType = "session.message"  // new message appended
	EventSessionCreated  EventType = "session.created"  // new session opened
	EventSessionKilled   EventType = "session.killed"   // messages cleared
	EventSessionDeleted  EventType = "session.deleted"  // session removed
	EventSessionAborted  EventType = "session.aborted"  // session aborted
	EventSessionReset    EventType = "session.reset"    // session reset
	EventSessionsChanged EventType = "sessions.changed" // any session list change

	// Agent events
	EventAgentReply   EventType = "agent.reply"      // assistant reply ready
	EventAgentRun     EventType = "agent.run"        // agent run started
	EventAgentRunDone EventType = "agent.run.done"   // agent run completed
	EventApproval     EventType = "approval.pending" // tool call awaiting approval

	// Tool / cron / hook events
	EventToolInvoked  EventType = "tool.invoked"  // a tool was executed
	EventCronFired    EventType = "cron.fired"    // cron job ran
	EventHookFired    EventType = "hook.fired"    // event hook dispatched
	EventPluginLoaded EventType = "plugin.loaded" // plugin loaded at startup

	// System events
	EventGatewayShutdown  EventType = "gateway.shutdown" // gateway stopping
	EventGatewayRestart   EventType = "gateway.restart"  // gateway restarting
	EventUpdateAvailable  EventType = "gateway.update_available"
	EventConnectChallenge EventType = "connect.challenge" // WS auth challenge
	EventPresence         EventType = "presence"          // client presence ping
	EventTick             EventType = "tick"              // periodic heartbeat event
	EventSystemEvent      EventType = "system.event"      // generic system event

	// Channel events
	EventChannelMessage EventType = "channel.message" // message via external channel
	EventChannelStarted EventType = "channel.started"
	EventChannelStopped EventType = "channel.stopped"

	// Config events
	EventConfigChanged EventType = "config.changed"
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
// Returns the event channel and an unsubscribe function.
//
// IMPORTANT: Do NOT close the returned channel — Publish() may hold a
// reference to it between reading the map and sending. Closing it while
// Publish() is still sending causes a panic. Instead, goroutine lifetimes
// must be bounded via a separate done/context signal (see dispatchWSFrame).
func (b *EventBus) Subscribe(sessionID string) (<-chan GatewayEvent, func()) {
	ch := make(chan GatewayEvent, 32)
	b.mu.Lock()
	b.seq++
	id := b.seq
	b.subs[id] = &subscriber{ch: ch, sessionID: sessionID}
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			// Drain the buffer so Publish() goroutines are never blocked
			// (channel is NOT closed — see note above).
			for len(ch) > 0 {
				<-ch
			}
		})
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

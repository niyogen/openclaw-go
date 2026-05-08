// Package channels — Nostr adapter (NIP-04 encrypted DMs via relay websocket).
// This is an outbound-only adapter for sending notes to a Nostr pubkey.
// Full NIP-04 encryption requires the sender's private key.
package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// NostrChannel sends messages to a Nostr relay as kind-1 text notes.
// For production use, replace with NIP-04 encrypted DMs.
type NostrChannel struct {
	relayURL string
	pubkey   string // destination pubkey (hex)
}

func NewNostrChannel(relayURL, pubkey string) *NostrChannel {
	return &NostrChannel{
		relayURL: strings.TrimSpace(relayURL),
		pubkey:   strings.TrimSpace(pubkey),
	}
}

func (n *NostrChannel) Name() string { return "nostr" }

func (n *NostrChannel) Send(ctx context.Context, message OutboundMessage) error {
	if n.relayURL == "" {
		return nil
	}
	target := strings.TrimSpace(message.Target)
	if target == "" {
		target = n.pubkey
	}
	if target == "" {
		return nil
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	conn, _, err := dialer.DialContext(ctx, n.relayURL, nil)
	if err != nil {
		return fmt.Errorf("nostr dial %s: %w", n.relayURL, err)
	}
	defer conn.Close()

	// Build a NIP-01 kind-1 text note EVENT.
	now := time.Now().Unix()
	event := []any{
		"EVENT",
		map[string]any{
			"kind":       1,
			"created_at": now,
			"tags":       [][]string{{"p", target}},
			"content":    message.Message,
			// Note: pubkey and id fields must be set by a real key-signing
			// implementation. Relays will reject unsigned events.
			"pubkey": "",
			"id":     "",
			"sig":    "",
		},
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, raw)
}

// NostrRelayConfig configures a Nostr relay connection for inbound listening.
type NostrRelayConfig struct {
	RelayURL string
	Pubkey   string // filter: only receive events tagged with this pubkey
}

// BuildNostrWebhookHandler is a placeholder — Nostr uses WebSocket relay
// subscriptions, not HTTP webhooks.  For inbound, connect to a relay
// separately and push events through the handler.  This function is provided
// for API consistency; it always returns 404.
func BuildNostrWebhookHandler(
	_ string,
	_ func(context.Context, InboundMessage) error,
) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"nostr uses relay WebSocket subscriptions, not HTTP webhooks"}`))
	}
}

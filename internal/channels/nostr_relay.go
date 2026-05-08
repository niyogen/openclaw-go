package channels

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// NostrEvent is a fully-formed NIP-01 event.
type NostrEvent struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"` // empty — relay rejects unsigned events; sign externally
}

// nostrEventID computes the NIP-01 event ID (sha256 of the canonical serialisation).
func nostrEventID(ev *NostrEvent) string {
	serialized, _ := json.Marshal([]any{
		0, ev.PubKey, ev.CreatedAt, ev.Kind, ev.Tags, ev.Content,
	})
	sum := sha256.Sum256(serialized)
	return hex.EncodeToString(sum[:])
}

// NewNostrEvent builds an unsigned kind-1 text note addressed to targetPubkey.
// The Sig field is intentionally left empty — real deployments must sign
// with the sender's private key using secp256k1 (e.g. via the btcec or
// decred libraries).
func NewNostrEvent(senderPubKey, targetPubKey, content string) *NostrEvent {
	ev := &NostrEvent{
		PubKey:    strings.TrimSpace(senderPubKey),
		CreatedAt: time.Now().Unix(),
		Kind:      1,
		Tags:      [][]string{{"p", strings.TrimSpace(targetPubKey)}},
		Content:   content,
	}
	ev.ID = nostrEventID(ev)
	return ev
}

// NostrRelaySubscription connects to a Nostr relay and listens for events
// matching the given filter, delivering them to handler as InboundMessages.
// It runs until ctx is cancelled.
func NostrRelaySubscription(
	ctx context.Context,
	relayURL string,
	filterPubKey string,
	handler func(context.Context, InboundMessage) error,
) error {
	if strings.TrimSpace(relayURL) == "" {
		return fmt.Errorf("relay URL is required")
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	conn, _, err := dialer.DialContext(ctx, relayURL, nil)
	if err != nil {
		return fmt.Errorf("nostr relay connect %s: %w", relayURL, err)
	}
	defer conn.Close()

	// Send REQ subscription.
	subID := fmt.Sprintf("sub-%d", time.Now().UnixNano())
	filter := map[string]any{"kinds": []int{1}}
	if strings.TrimSpace(filterPubKey) != "" {
		filter["#p"] = []string{filterPubKey}
	}
	reqMsg, _ := json.Marshal([]any{"REQ", subID, filter})
	if err := conn.WriteMessage(websocket.TextMessage, reqMsg); err != nil {
		return fmt.Errorf("nostr REQ send: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			// Send CLOSE before exiting.
			closeMsg, _ := json.Marshal([]any{"CLOSE", subID})
			_ = conn.WriteMessage(websocket.TextMessage, closeMsg)
			return ctx.Err()
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("nostr relay read: %w", err)
		}

		var envelope []json.RawMessage
		if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope) < 3 {
			continue
		}
		var msgType string
		if err := json.Unmarshal(envelope[0], &msgType); err != nil || msgType != "EVENT" {
			continue
		}
		var ev NostrEvent
		if err := json.Unmarshal(envelope[2], &ev); err != nil {
			continue
		}
		text := strings.TrimSpace(ev.Content)
		if text == "" {
			continue
		}
		_ = handler(ctx, InboundMessage{
			SessionID: "nostr:" + ev.PubKey,
			Channel:   "nostr",
			Target:    ev.PubKey,
			Message:   text,
		})
	}
}

package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// MatrixChannel sends messages to a Matrix homeserver via the Client-Server
// API (PUT /_matrix/client/v3/rooms/{roomId}/send/m.room.message/{txnId}).
//
// Inbound (`/sync` long-poll) is deferred: it requires a stateful poller
// with token persistence and is sizable. Users wanting inbound today pair
// Matrix-out with another channel.
type MatrixChannel struct {
	baseURL     string // e.g. "https://matrix.example.com"
	accessToken string // bot's Matrix access token
	client      *http.Client
	txnCounter  atomic.Uint64 // monotonic per-process txn-id
}

// NewMatrixChannel constructs the channel. Empty baseURL OR accessToken
// disables Send (returns nil) — matches the disabled-channel pattern.
func NewMatrixChannel(baseURL, accessToken string) *MatrixChannel {
	return &MatrixChannel{
		baseURL:     strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		accessToken: strings.TrimSpace(accessToken),
		client:      &http.Client{Timeout: 20 * time.Second},
	}
}

func (m *MatrixChannel) Name() string {
	return "matrix"
}

// Send posts a message into the room identified by message.Target. Target
// is the canonical Matrix room ID ("!opaque:example.com") — NOT an alias
// (#name:example.com). If you have an alias, resolve it first via
// `/_matrix/client/v3/directory/room/{alias}`.
func (m *MatrixChannel) Send(ctx context.Context, message OutboundMessage) error {
	if m.baseURL == "" || m.accessToken == "" {
		return nil // disabled
	}
	roomID := strings.TrimSpace(message.Target)
	if roomID == "" {
		return fmt.Errorf("matrix: target (room id) is required")
	}
	if !strings.HasPrefix(roomID, "!") {
		return fmt.Errorf("matrix: target must be a room id starting with '!', got %q (resolve aliases first)", roomID)
	}

	body := map[string]string{
		"msgtype": "m.text",
		"body":    message.Message,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	// Per Matrix spec, the txn id MUST be unique per access token per
	// request to make the PUT idempotent — replays of the same txn id are
	// no-ops on the homeserver.
	txn := m.nextTxnID()
	endpoint := fmt.Sprintf(
		"%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		m.baseURL,
		url.PathEscape(roomID),
		url.PathEscape(txn),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.accessToken)
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("matrix: PUT %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("matrix: %s returned %d: %s", endpoint, resp.StatusCode, string(respBody))
	}
	return nil
}

// nextTxnID returns a per-process-unique transaction id. Format is
// `oc-<unix-nano>-<counter>` so two processes don't collide and replays
// within one process map to the same id.
func (m *MatrixChannel) nextTxnID() string {
	n := m.txnCounter.Add(1)
	return "oc-" + strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.FormatUint(n, 36)
}

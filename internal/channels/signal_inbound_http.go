package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SignalHTTPFetcher is the production SignalFetcher: it long-polls
// signal-cli-rest-api's GET /v1/receive/{number} endpoint, parses the
// envelope JSON, and filters out non-message events (typing indicators,
// read receipts, etc.) before returning.
//
// Why GET /v1/receive and not GET /v2/receive: v2 of the API returns a
// streaming response that requires a long-lived connection and Server-Sent
// Events parsing. v1 returns a JSON array on each call and is easier to
// reason about — the poller loop handles repeated calls just fine.
type SignalHTTPFetcher struct {
	baseURL        string // e.g. "http://127.0.0.1:8080"
	number         string // the bot's own number — required by the endpoint
	receiveTimeout time.Duration
	client         *http.Client
}

// NewSignalHTTPFetcher wires baseURL + number with a configurable receive
// timeout (passed as `?timeout=Ns`). The HTTP client timeout is set to
// receiveTimeout + 5s so the server-side long-poll always has a chance to
// return naturally before the client gives up.
func NewSignalHTTPFetcher(baseURL, number string, receiveTimeout time.Duration) *SignalHTTPFetcher {
	if receiveTimeout < time.Second {
		receiveTimeout = 5 * time.Second
	}
	if receiveTimeout > 60*time.Second {
		receiveTimeout = 60 * time.Second
	}
	return &SignalHTTPFetcher{
		baseURL:        strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		number:         strings.TrimSpace(number),
		receiveTimeout: receiveTimeout,
		client: &http.Client{
			Timeout: receiveTimeout + 5*time.Second,
		},
	}
}

// signalEnvelopeWire is the on-the-wire shape of one element in the
// /v1/receive response array. We decode only the fields we care about and
// let json.Unmarshal silently drop the rest.
type signalEnvelopeWire struct {
	Envelope struct {
		Source      string `json:"source"`
		SourceName  string `json:"sourceName"`
		Timestamp   int64  `json:"timestamp"`
		DataMessage *struct {
			Message   string `json:"message"`
			GroupInfo *struct {
				GroupID string `json:"groupId"`
			} `json:"groupInfo"`
		} `json:"dataMessage"`
	} `json:"envelope"`
}

// FetchNew performs one long-poll call. signal-cli-rest-api consumes the
// messages as it returns them — there's no separate ack step.
func (f *SignalHTTPFetcher) FetchNew(ctx context.Context) ([]SignalInboundMessage, error) {
	if f.baseURL == "" || f.number == "" {
		return nil, fmt.Errorf("signal: fetcher misconfigured (baseURL or number empty)")
	}
	u := f.baseURL + "/v1/receive/" + url.PathEscape(f.number)
	q := url.Values{}
	q.Set("timeout", strconv.Itoa(int(f.receiveTimeout.Seconds())))
	u += "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("signal: GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("signal: %s returned %d: %s", u, resp.StatusCode, string(body))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("signal: read body: %w", err)
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil, nil
	}
	var wire []signalEnvelopeWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("signal: decode envelope: %w", err)
	}
	out := make([]SignalInboundMessage, 0, len(wire))
	for _, w := range wire {
		if w.Envelope.DataMessage == nil {
			continue // typing indicator, read receipt, etc.
		}
		msg := strings.TrimSpace(w.Envelope.DataMessage.Message)
		if msg == "" {
			continue // empty body (reactions, attachments-only — out of v1 scope)
		}
		var groupID string
		if w.Envelope.DataMessage.GroupInfo != nil {
			groupID = w.Envelope.DataMessage.GroupInfo.GroupID
		}
		out = append(out, SignalInboundMessage{
			Source:    strings.TrimSpace(w.Envelope.Source),
			Message:   msg,
			GroupID:   strings.TrimSpace(groupID),
			Timestamp: w.Envelope.Timestamp,
		})
	}
	return out, nil
}

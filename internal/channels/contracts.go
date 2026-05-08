package channels

import "context"

type InboundMessage struct {
	SessionID string `json:"sessionId"`
	Channel   string `json:"channel"`
	Target    string `json:"target"`
	Message   string `json:"message"`
}

type OutboundMessage struct {
	SessionID string `json:"sessionId"`
	Channel   string `json:"channel"`
	Target    string `json:"target"`
	Message   string `json:"message"`
	// ThreadID optionally specifies a thread or reply token for channel-specific threading.
	// Slack: thread_ts, Teams: replyToId, LINE: replyToken.
	ThreadID string `json:"threadId,omitempty"`
	// MediaURL is an optional image/file attachment URL.
	MediaURL string `json:"mediaUrl,omitempty"`
}

type Channel interface {
	Name() string
	Send(ctx context.Context, message OutboundMessage) error
}

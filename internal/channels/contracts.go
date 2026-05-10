package channels

import "context"

// WebhookInboundConfig configures optional behavior for channel webhook HTTP handlers.
type WebhookInboundConfig struct {
	// OnHandlerError is invoked when the application inbound handler returns a
	// non-nil error (after the webhook body passed decode/signature checks).
	OnHandlerError func(channel string, err error, attrs map[string]any)
}

type InboundMessage struct {
	SessionID string `json:"sessionId"`
	Channel   string `json:"channel"`
	Target    string `json:"target"`
	Message   string `json:"message"`
}

// Button is an interactive button for channel messages (Slack, Discord).
type Button struct {
	Label  string `json:"label"`
	Value  string `json:"value"`
	Style  string `json:"style,omitempty"` // "primary", "danger", "default"
	Action string `json:"action,omitempty"`
}

// Reaction represents an emoji reaction to add to a message.
type Reaction struct {
	Emoji     string `json:"emoji"` // e.g. "thumbsup", "heart"
	MessageID string `json:"messageId,omitempty"`
}

type OutboundMessage struct {
	SessionID string `json:"sessionId"`
	Channel   string `json:"channel"`
	Target    string `json:"target"`
	Message   string `json:"message"`
	// ThreadID optionally specifies a thread or reply token for channel-specific threading.
	// Slack: thread_ts, Teams: replyToId, LINE: replyToken.
	ThreadID string `json:"threadId,omitempty"`
	// ReplyToMessageID optionally replies to a prior message (Telegram: reply_to_message_id).
	ReplyToMessageID string `json:"replyToMessageId,omitempty"`
	// MediaURL is an optional image/file attachment URL.
	MediaURL string `json:"mediaUrl,omitempty"`
	// Buttons are interactive action buttons (Slack Block Kit, Discord components).
	Buttons []Button `json:"buttons,omitempty"`
	// Reactions are emoji reactions to add after posting.
	Reactions []Reaction `json:"reactions,omitempty"`
	// Ephemeral means the message is only visible to the target user (Slack).
	Ephemeral bool `json:"ephemeral,omitempty"`
}

type Channel interface {
	Name() string
	Send(ctx context.Context, message OutboundMessage) error
}

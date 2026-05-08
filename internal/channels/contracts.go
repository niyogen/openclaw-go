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
}

type Channel interface {
	Name() string
	Send(ctx context.Context, message OutboundMessage) error
}

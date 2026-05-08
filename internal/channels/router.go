package channels

import (
	"context"
	"strings"
)

type Router struct {
	items map[string]Channel
}

func NewRouter() *Router {
	return &Router{
		items: map[string]Channel{},
	}
}

func (r *Router) Register(ch Channel) {
	if ch == nil {
		return
	}
	r.items[strings.ToLower(ch.Name())] = ch
}

func (r *Router) Dispatch(ctx context.Context, message OutboundMessage) error {
	channelName := strings.ToLower(strings.TrimSpace(message.Channel))
	if channelName == "" {
		return nil
	}
	ch, ok := r.items[channelName]
	if !ok {
		return nil
	}
	return ch.Send(ctx, message)
}

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

// Names returns the names of all registered channels.
func (r *Router) Names() []string {
	names := make([]string, 0, len(r.items))
	for k := range r.items {
		names = append(names, k)
	}
	return names
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

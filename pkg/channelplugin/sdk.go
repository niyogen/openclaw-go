// Package channelplugin is the SDK plugin authors import when building a
// channel plugin for openclaw-go. It hides the on-the-wire contract
// (HTTP shapes, auth headers, env vars) behind a small struct API:
//
//	func main() {
//	    p, err := channelplugin.LoadFromEnv()
//	    if err != nil { log.Fatal(err) }
//	    p.OnSend = func(ctx context.Context, msg channelplugin.OutboundMessage) error {
//	        // send msg.Message to msg.Target via your platform's API
//	        return nil
//	    }
//	    // When your platform delivers an inbound message, call p.DispatchInbound:
//	    go yourPlatformPoller(p)
//	    log.Fatal(p.Listen(":9101"))
//	}
//
// Operator-side config travels via three env vars set when launching the
// plugin process (the gateway prints them after approve):
//
//	OPENCLAW_PLUGIN_NAME    — must match manifest.name
//	OPENCLAW_GATEWAY_URL    — e.g. http://127.0.0.1:18789
//	OPENCLAW_PLUGIN_TOKEN   — issued via `openclaw plugins approve <name>`
//
// See docs/PLUGIN-ARCHITECTURE.md for the full contract.
package channelplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// OutboundMessage mirrors channels.OutboundMessage in the gateway. Kept
// as a parallel type so plugins can import this package without pulling
// in `internal/`. JSON tags must match exactly — the gateway encodes
// using the original type, the plugin decodes using this one.
type OutboundMessage struct {
	SessionID        string     `json:"sessionId"`
	Channel          string     `json:"channel"`
	Target           string     `json:"target"`
	Message          string     `json:"message"`
	ThreadID         string     `json:"threadId,omitempty"`
	ReplyToMessageID string     `json:"replyToMessageId,omitempty"`
	MediaURL         string     `json:"mediaUrl,omitempty"`
	Buttons          []Button   `json:"buttons,omitempty"`
	Reactions        []Reaction `json:"reactions,omitempty"`
	Ephemeral        bool       `json:"ephemeral,omitempty"`
}

// InboundMessage mirrors channels.InboundMessage. Plugins build one and
// POST it back via Plugin.DispatchInbound.
type InboundMessage struct {
	SessionID string `json:"sessionId"`
	Channel   string `json:"channel"`
	Target    string `json:"target"`
	Message   string `json:"message"`
}

// Button mirrors channels.Button for plugins that surface interactive UI.
type Button struct {
	Label  string `json:"label"`
	Value  string `json:"value"`
	Style  string `json:"style,omitempty"`
	Action string `json:"action,omitempty"`
}

// Reaction mirrors channels.Reaction.
type Reaction struct {
	Emoji     string `json:"emoji"`
	MessageID string `json:"messageId,omitempty"`
}

// Plugin is the operator-facing handle. Construct via LoadFromEnv (or
// fill the struct yourself for tests). Call Listen to start serving
// outbound dispatches; call DispatchInbound from your platform poller
// to deliver received messages to the gateway.
type Plugin struct {
	// Name MUST match the `name` field in your plugin.json manifest.
	Name string
	// GatewayURL is where the gateway listens (typically
	// http://127.0.0.1:18789 in local deployments).
	GatewayURL string
	// Token is the bearer token issued by `openclaw plugins approve`.
	// Treat as secret — anyone with the token can dispatch messages
	// into the gateway under your plugin's identity.
	Token string
	// OnSend is invoked for every outbound message the gateway dispatches
	// to this plugin. Return nil for success; any non-nil error is
	// surfaced as a 500 to the gateway's router (and triggers retry +
	// the channel_dispatch_errors_total metric).
	OnSend func(ctx context.Context, msg OutboundMessage) error
	// Client is the HTTP client used for inbound dispatches. nil = a
	// default *http.Client with a 30s timeout.
	Client *http.Client
}

// LoadFromEnv reads the three required env vars and returns a configured
// Plugin (without OnSend or Listen wired). Returns a clear error when
// any var is missing — fail loudly at startup rather than silently
// dropping every message.
func LoadFromEnv() (*Plugin, error) {
	name := strings.TrimSpace(os.Getenv("OPENCLAW_PLUGIN_NAME"))
	gw := strings.TrimSpace(os.Getenv("OPENCLAW_GATEWAY_URL"))
	tok := strings.TrimSpace(os.Getenv("OPENCLAW_PLUGIN_TOKEN"))
	missing := []string{}
	if name == "" {
		missing = append(missing, "OPENCLAW_PLUGIN_NAME")
	}
	if gw == "" {
		missing = append(missing, "OPENCLAW_GATEWAY_URL")
	}
	if tok == "" {
		missing = append(missing, "OPENCLAW_PLUGIN_TOKEN")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return &Plugin{Name: name, GatewayURL: gw, Token: tok}, nil
}

// Listen starts the plugin's HTTP server at addr. Blocks until the
// listener exits. /channel/send is wired to p.OnSend automatically; any
// extra handlers can be added via your own mux + http.Serve before
// calling this.
func (p *Plugin) Listen(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/channel/send", p.handleSend)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return server.ListenAndServe()
}

// Handler returns the http.Handler the plugin would serve. Useful for
// composing into a larger HTTP server with additional routes (health,
// metrics, etc.).
func (p *Plugin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/channel/send", p.handleSend)
	return mux
}

func (p *Plugin) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var msg OutboundMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if p.OnSend == nil {
		http.Error(w, "plugin has no OnSend handler", http.StatusNotImplemented)
		return
	}
	if err := p.OnSend(r.Context(), msg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// DispatchInbound POSTs an InboundMessage to the gateway, authenticated
// with the plugin's bearer token. Called by the plugin author's platform
// poller / webhook handler whenever a new message arrives.
//
// The gateway overwrites msg.Channel with the manifest's channel name —
// callers don't need to set it correctly, but setting it consistently
// is good practice for log clarity.
func (p *Plugin) DispatchInbound(ctx context.Context, msg InboundMessage) error {
	if p.Name == "" || p.GatewayURL == "" || p.Token == "" {
		return errors.New("plugin not configured (run LoadFromEnv first)")
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	url := strings.TrimRight(p.GatewayURL, "/") + "/plugins/" + p.Name + "/inbound"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.Token)
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gateway returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

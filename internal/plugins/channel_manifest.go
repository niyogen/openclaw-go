package plugins

// ChannelManifest is the per-plugin declaration that an external process
// implements a channel. When present on a Manifest, the gateway treats the
// plugin as a registered channels.Channel and routes outbound dispatches to
// its BaseURL/channel/send endpoint.
//
// Operator-edited; lives in plugin.json. See docs/PLUGIN-ARCHITECTURE.md
// for the full contract.
type ChannelManifest struct {
	// Channel is the name the plugin registers under (e.g. "telegram",
	// "bluesky", "custom-discord"). Must be unique across all enabled
	// channels — if a built-in channel uses the same name, gateway init
	// refuses to start.
	Channel string `json:"channel"`

	// BaseURL is where the plugin's HTTP server listens. Typically a
	// loopback address like "http://127.0.0.1:9101" because plugins run
	// as sidecars on the same host. Public/remote BaseURLs are allowed
	// but operators should put TLS in front and review the threat model
	// first (the per-plugin token is the only authentication on the
	// inbound callback channel).
	BaseURL string `json:"baseUrl"`
}

// HasChannelPlugin reports whether the manifest declares a channel plugin.
// Used by the gateway init to decide whether to register a pluginChannel.
func (m Manifest) HasChannelPlugin() bool {
	if m.Channel == nil {
		return false
	}
	if m.Channel.Channel == "" || m.Channel.BaseURL == "" {
		return false
	}
	return true
}

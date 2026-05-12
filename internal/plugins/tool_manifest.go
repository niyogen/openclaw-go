package plugins

import "strings"

// HasToolPlugin reports whether the manifest declares one or more tool
// endpoints under the existing Tools[] field. The Tools field has lived
// on Manifest since the original plugin loader landed; this helper just
// makes the "is this a tool plugin?" check explicit so the tool-plugin
// registry can use the same pattern as ChannelPluginRegistry.
//
// A tool entry is considered valid when both Name and Endpoint are
// non-empty after trimming. Empty/garbage entries are ignored so a
// manifest with [valid, "", valid] still counts as a tool plugin.
func (m Manifest) HasToolPlugin() bool {
	for _, t := range m.Tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		if strings.TrimSpace(t.Endpoint) == "" {
			continue
		}
		return true
	}
	return false
}

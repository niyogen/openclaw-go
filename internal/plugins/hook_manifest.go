package plugins

import "strings"

// HasHookPlugin reports whether the manifest declares one or more hook
// subscriptions under the Hooks[] field. Mirrors HasToolPlugin /
// HasChannelPlugin shape so the hook-plugin registry can apply the
// same scan/approve/dispatch pattern.
//
// An entry is valid when both Event and Endpoint are non-empty after
// trimming. Garbage entries are ignored.
func (m Manifest) HasHookPlugin() bool {
	for _, h := range m.Hooks {
		if strings.TrimSpace(h.Event) == "" {
			continue
		}
		if strings.TrimSpace(h.Endpoint) == "" {
			continue
		}
		return true
	}
	return false
}

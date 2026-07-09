package delegate

import (
	"slices"
	"strings"
)

// watchCapabilityPrefix flags a capability as addressing the watch
// subsystem (watch.subscribe / watch.unsubscribe).
const watchCapabilityPrefix = "watch."

// HasWatchCapability reports whether the granted-cap list contains any
// `watch.*` entry. Watch tools are currently wired for the claw backend
// only (see pkg/backend/tool/claw_watch_tools.go); the claude_code path
// uses this to warn rather than silently drop the capability.
func HasWatchCapability(caps []string) bool {
	return hasCapabilityPrefix(caps, watchCapabilityPrefix)
}

// hasCapabilityPrefix reports whether caps contains an entry starting
// with prefix (e.g. "board." or "watch.").
func hasCapabilityPrefix(caps []string, prefix string) bool {
	return slices.ContainsFunc(caps, func(c string) bool {
		return strings.HasPrefix(c, prefix)
	})
}

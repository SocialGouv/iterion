package delegate

import (
	"slices"
	"strings"
)

// watchCapabilityPrefix flags a capability as addressing the watch
// subsystem (watch.subscribe / watch.unsubscribe).
const watchCapabilityPrefix = "watch."

// WatchMCPServerName is the server iterion's watch tools register under
// (claw: mcp.iterion_watch.*; claude_code FQN mcp__iterion_watch__*).
const WatchMCPServerName = "iterion_watch"

// watchTools maps each watch tool to the capability that opens it: the one
// table the claw registrar and the grant check (HostToolGranted) both read.
// The capability names are ir.CapWatchSubscribe / ir.CapWatchUnsubscribe (the
// validator's source of truth), spelled here because this package does not
// import the IR; a test pins the two together.
var watchTools = []struct{ name, capability string }{
	{"subscribe", "watch.subscribe"},
	{"unsubscribe", "watch.unsubscribe"},
}

// WatchToolCapability returns the capability that opens the named watch
// tool, or "" when the name is not one.
func WatchToolCapability(tool string) string {
	for _, t := range watchTools {
		if t.name == tool {
			return t.capability
		}
	}
	return ""
}

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

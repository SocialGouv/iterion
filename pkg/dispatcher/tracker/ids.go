package tracker

import (
	"fmt"
	"strings"
)

// parsePrefixedID parses a "<prefix><n>" identifier and returns n. Returns
// false if id doesn't start with prefix, the remainder isn't a valid
// integer, or the integer isn't positive — rejecting e.g. a "#-5" suffix
// that would otherwise decode into a flag-injection-shaped negative number
// passed on to a CLI arg or URL path segment downstream.
func parsePrefixedID(prefix, id string) (int, bool) {
	if !strings.HasPrefix(id, prefix) {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimPrefix(id, prefix), "%d", &n); err != nil {
		return 0, false
	}
	if n <= 0 {
		return 0, false
	}
	return n, true
}

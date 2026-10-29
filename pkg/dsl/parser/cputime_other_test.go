//go:build !unix

package parser

import (
	"testing"
	"time"
)

// processCPUTime has no reading off unix: it reports false, and a timing
// claim built on it is left unchecked rather than read off the wall clock,
// which on a shared runner measures the machine's load.
func processCPUTime(t testing.TB) (time.Duration, bool) {
	t.Helper()
	return 0, false
}

//go:build !unix

package e2e

import (
	"os/exec"
	"testing"
	"time"
)

// childCPU: no rusage off unix — the caller falls back to its wall-clock
// guard alone.
func childCPU(t *testing.T, _ *exec.Cmd) (time.Duration, bool) {
	t.Helper()
	return 0, false
}

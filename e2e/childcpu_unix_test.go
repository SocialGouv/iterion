//go:build unix

package e2e

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// childCPU is the user+system CPU the reaped child burned — load-immune
// where the wall clock is not: a starved runner stretches the wall, but
// the OS schedules the child, so competing Go tests do not multiply its
// work. ok is false when the platform reports no usage.
func childCPU(t *testing.T, c *exec.Cmd) (time.Duration, bool) {
	t.Helper()
	ru, ok := c.ProcessState.SysUsage().(*syscall.Rusage)
	if !ok {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), true
}

//go:build unix

package runview

import (
	"errors"
	"syscall"
)

// newDetachedSysProcAttr puts the spawned runner in its own session.
// A SIGTERM delivered to the server's PGID does NOT propagate to the
// runner; signalling `-pid` from the server reaches the runner plus
// every descendant it forks (claude_code, codex, MCP servers).
func newDetachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setsid: true,
	}
}

// terminateProcessGroup delivers SIGTERM to every member of pid's
// process group. Negative-PID semantics on syscall.Kill is the Unix
// idiom for "the whole group" — see newDetachedSysProcAttr above for
// why the runner is in its own session.
func terminateProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGTERM)
}

// pidAliveOS implements pidAlive for Unix using kill -0. ESRCH means
// the process no longer exists; everything else (typically EPERM) is
// returned as-is so the reconciler can decide what to do.
func pidAliveOS(pid int) error {
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return errProcessNotFound
		}
		return err
	}
	return nil
}

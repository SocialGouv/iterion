//go:build unix

package operatormcp

import (
	"errors"
	"syscall"
)

// detachedSysProcAttr puts the spawned runner in its own session so a
// signal to this server's process group never propagates to it —
// the run must outlive the MCP client's session.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// terminateProcessGroup delivers SIGTERM to every member of pid's
// process group (negative-PID kill semantics), reaching the runner
// plus every descendant it forked (claude_code, MCP servers, tools).
func terminateProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGTERM)
}

// pidAlive reports nil when pid exists, errProcessNotFound when it is
// gone (ESRCH), and the raw error otherwise (typically EPERM, which
// means "someone else's process" and should read as alive).
func pidAlive(pid int) error {
	if pid <= 0 {
		return errProcessNotFound
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return errProcessNotFound
		}
		return err
	}
	return nil
}

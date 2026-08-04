//go:build windows

package operatormcp

import (
	"fmt"
	"syscall"
)

// detachedSysProcAttr is a Windows stub: no session/process-group
// detachment (mirrors runview's detached_windows.go — detached-run
// support is Unix-first).
func detachedSysProcAttr() *syscall.SysProcAttr { return nil }

// terminateProcessGroup is unsupported on Windows.
func terminateProcessGroup(pid int) error {
	return fmt.Errorf("operatormcp: cancelling a detached run is not supported on windows (pid %d)", pid)
}

// pidAlive conservatively reports "not found" on Windows, matching the
// runview stub: liveness probing is Unix-only.
func pidAlive(pid int) error { return errProcessNotFound }

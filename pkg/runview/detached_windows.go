//go:build windows

package runview

import "syscall"

// newDetachedSysProcAttr is the Windows fallback. The editor's detached
// runner mode is not validated on Windows; release.yml still cross-
// compiles a windows/{amd64,arm64} binary, so these helpers exist to
// keep the package building. A future Windows port should swap them
// for real implementations using the Win32 console-control APIs
// (GenerateConsoleCtrlEvent + OpenProcess).
func newDetachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: 0x00000200, // CREATE_NEW_PROCESS_GROUP
	}
}

// terminateProcessGroup is a Windows stub. Returning errProcessNotFound
// makes callers treat the run as gone — the safest default until a
// real CTRL_BREAK_EVENT path is wired up.
func terminateProcessGroup(pid int) error {
	return errProcessNotFound
}

// pidAliveOS is a Windows stub. Same rationale as terminateProcessGroup.
func pidAliveOS(pid int) error {
	return errProcessNotFound
}

//go:build windows

package claudesdk

import (
	"os/exec"
	"syscall"
)

// setProcessGroup is a no-op on Windows. The runtime currently targets
// Linux/macOS for the long-running editor/run path; if Windows desktop ever
// needs robust subtree termination this should switch to
// CREATE_NEW_PROCESS_GROUP and route TerminateJobObject through a Job Object.
func setProcessGroup(_ *exec.Cmd) {}

// killProcessGroup falls back to terminating the leader on Windows.
// Descendants are left to the caller; see setProcessGroup for the path
// forward.
func killProcessGroup(pid int, _ syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	p, err := exec.LookPath("taskkill")
	if err == nil {
		// /T = tree, /F = force.
		_ = exec.Command(p, "/F", "/T", "/PID", itoa(pid)).Run()
	}
	return nil
}

func itoa(n int) string {
	// Avoid pulling strconv just for this; n is always positive here.
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

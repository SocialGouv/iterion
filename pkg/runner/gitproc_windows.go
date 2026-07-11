//go:build windows

package runner

import "os/exec"

// hardenGitCancel is a no-op on Windows: there is no process-group kill
// via negative PID; the caller's WaitDelay remains the unblock for
// helpers holding the output pipes after the direct child is killed.
func hardenGitCancel(_ *exec.Cmd) {}

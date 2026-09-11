//go:build unix

package server

import (
	"os/exec"
	"syscall"
	"time"
)

// configureAuthoringPythonCommand keeps a parser timeout from leaving a
// descendant process behind. The parser program is constant and should not
// spawn children, but the process-group boundary makes that invariant safe if
// the interpreter changes.
func configureAuthoringPythonCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 500 * time.Millisecond
}

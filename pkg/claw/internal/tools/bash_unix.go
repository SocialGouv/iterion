//go:build !windows

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

// applyBashProcessGroup runs the bash child in its own process group so
// a context-cancel can SIGKILL the whole tree, not just the immediate
// shell. The grandchild-pipe problem (orphaned descendant keeps stdout
// open, blocking cmd.Wait) is also covered by cmd.WaitDelay set at the
// callsite; this function handles the kill-the-tree half.
func applyBashProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			// Negative PID = signal the group, not just the leader.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return os.ErrProcessDone
	}
}

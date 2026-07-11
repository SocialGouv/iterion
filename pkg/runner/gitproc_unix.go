//go:build unix

package runner

import (
	"os/exec"
	"syscall"
)

// hardenGitCancel makes ctx cancellation actually terminate a git
// invocation. Two gaps in the default exec.CommandContext behaviour:
//   - git spawns helpers (git-remote-https, ssh) that inherit our
//     stdout/stderr pipes; killing only the direct child leaves
//     CombinedOutput blocked on the helper's copy of the pipes, so the
//     timeout "fires" but runGit never returns. Kill the whole process
//     group instead (the child is made its own group leader).
//   - even then a pathological helper can hold the pipes; WaitDelay is
//     set by the caller as the final unblock.
func hardenGitCancel(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

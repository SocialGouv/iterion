//go:build !windows

package hooks

import (
	"os/exec"
	"syscall"
)

// shellArgs returns ("sh", ["-lc", command]) for Unix.
func shellArgs(command string) (string, []string) {
	return "sh", []string{"-lc", command}
}

// setProcGroup puts the child shell into its own process group so we can
// signal the whole group on cancellation. Without this, killing the shell
// leaves orphaned grandchildren (e.g. `sleep`) that keep stdout/stderr open
// and block `cmd.Wait()` until they exit naturally.
func setProcGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessTree sends SIGKILL to the process group rooted at the child
// shell. Falls back to killing the leader if the group kill fails.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

//go:build unix

package proc

import (
	"os/exec"
	"syscall"
)

// DetachProcessGroup makes the subprocess the leader of its own
// process group so a SIGTERM to the parent's PGID doesn't propagate
// to it. Pre-Start; safe to call on any *exec.Cmd.
func DetachProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

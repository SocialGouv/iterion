//go:build unix && !linux

package model

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureToolNodeProcessGroup gives a host-executed shell/script recipe its
// own process group and makes context cancellation terminate the whole tree.
//
// exec.CommandContext's default Cancel kills only cmd.Process. That is not
// sufficient for tool recipes: a Python script commonly starts another costly
// CLI (Codex, Blender, Tripo helpers, ...), and killing Python alone reparents
// that child to PID 1. The workflow then reaches a terminal state while the
// detached child keeps running and spending resources.
//
// SIGKILL deliberately matches CommandContext's existing cancellation
// semantics; the only change is its scope (the command's process group instead
// of just the group leader). Normal command completion is unaffected.
func configureToolNodeProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil || cmd.Process.Pid <= 0 {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}

//go:build !windows

package subprocess

import (
	stderrors "errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

func killProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	pid := cmd.Process.Pid

	pgid, err := syscall.Getpgid(pid)
	if err == nil {
		if killErr := syscall.Kill(-pgid, syscall.SIGKILL); killErr == nil {
			return nil
		} else if !stderrors.Is(killErr, syscall.ESRCH) {
			return fmt.Errorf("kill Codex process group (pgid %d): %w", pgid, killErr)
		}
	} else if !stderrors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("get Codex process group (pid %d): %w", pid, err)
	}

	if err := cmd.Process.Kill(); err != nil && !stderrors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill Codex process (pid %d): %w", pid, err)
	}

	return nil
}

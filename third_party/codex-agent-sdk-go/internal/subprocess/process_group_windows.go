//go:build windows

package subprocess

import (
	stderrors "errors"
	"fmt"
	"os"
	"os/exec"
)

func configureProcessGroup(_ *exec.Cmd) {}

func killProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	if err := cmd.Process.Kill(); err != nil && !stderrors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill Codex process (pid %d): %w", cmd.Process.Pid, err)
	}

	return nil
}

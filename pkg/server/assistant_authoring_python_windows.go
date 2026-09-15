//go:build windows

package server

import (
	"os/exec"
	"time"
)

func configureAuthoringPythonCommand(cmd *exec.Cmd) {
	cmd.WaitDelay = 500 * time.Millisecond
}

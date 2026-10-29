//go:build windows

package server

import (
	"os/exec"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

func configureAuthoringPythonCommand(cmd *exec.Cmd) {
	proc.SetWaitDelay(cmd, authoringPythonDrainBound)
}

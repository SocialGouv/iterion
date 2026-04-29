//go:build windows

package hooks

import (
	"os/exec"
	"strconv"
)

// shellArgs returns ("cmd", ["/C", command]) for Windows.
func shellArgs(command string) (string, []string) {
	return "cmd", []string{"/C", command}
}

// setProcGroup is a no-op on Windows; cmd.exe child trees are handled by
// killProcessTree below via taskkill.
func setProcGroup(cmd *exec.Cmd) {}

// killProcessTree terminates the cmd.exe child and its descendants using
// taskkill /T /F. Falls back to killing only the leader if taskkill fails.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pidStr := strconv.Itoa(cmd.Process.Pid)
	if err := exec.Command("taskkill", "/T", "/F", "/PID", pidStr).Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}

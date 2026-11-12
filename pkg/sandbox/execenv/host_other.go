//go:build !linux

package execenv

import (
	"io"
	"os/exec"
)

func hostSupported() error           { return refusal("host", "unsupported-platform", nil) }
func configureHostCommand(*exec.Cmd) {}
func runHost(wireLaunch, []string, io.Reader, io.Writer, io.Writer) (int, error) {
	return 125, hostSupported()
}

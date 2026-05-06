//go:build windows

package kubernetes

import "os/exec"

// detachProcessGroup is a Windows no-op. The kubernetes driver is
// only selected when the process is running in-cluster (a Linux
// pod), so this branch is unreachable in practice — kept for build
// portability.
func detachProcessGroup(_ *exec.Cmd) {}

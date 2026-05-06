//go:build windows

package runtime

import "os/exec"

// detachGitProcessGroup is a no-op on Windows. The Unix process-group
// model doesn't apply, and the watchexec/SIGTERM regression that this
// guard exists for cannot occur there. Job Objects would be the
// equivalent if signalling parity were ever needed.
func detachGitProcessGroup(_ *exec.Cmd) {}

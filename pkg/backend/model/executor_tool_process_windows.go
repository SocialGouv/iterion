//go:build windows

package model

import "os/exec"

// configureToolNodeProcessGroup keeps exec.CommandContext's leader-only
// cancellation on Windows. A complete port should use a Job Object so child
// processes are terminated together; the Unix implementation covers the local
// Studio lifecycle where this regression was observed.
func configureToolNodeProcessGroup(_ *exec.Cmd) {}

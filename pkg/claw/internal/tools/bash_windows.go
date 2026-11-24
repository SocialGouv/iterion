//go:build windows

package tools

import "os/exec"

// applyBashProcessGroup is a no-op on Windows: syscall.SysProcAttr.Setpgid
// and syscall.Kill are Unix-only, and the bash tool's primary use case
// (running shell commands during agent workflows) is gated on a bash
// executable being on PATH — typical Windows deployments rely on Git
// Bash or WSL, both of which provide their own job-control semantics.
// The cmd.WaitDelay set at the callsite still applies and keeps Wait
// from hanging on orphaned descendants.
func applyBashProcessGroup(_ *exec.Cmd) {}

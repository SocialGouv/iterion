//go:build unix

package runview

import "syscall"

// newDetachedSysProcAttr puts the spawned runner in its own session.
// A SIGTERM delivered to the server's PGID does NOT propagate to the
// runner; signalling `-pid` from the server reaches the runner plus
// every descendant it forks (claude_code, codex, MCP servers).
func newDetachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setsid: true,
	}
}

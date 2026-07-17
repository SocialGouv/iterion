//go:build !windows

// Package runshell spawns interactive post-mortem shells in preserved
// run worktrees (the studio's "Open shell" on a failed run). One
// Session per WebSocket connection — the worktree is the state, the
// shell is a viewer: no persistent multiplexer, a reconnect is a fresh
// shell. Unix-only; the Windows build carries a typed stub.
package runshell

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

// SpawnOptions configures one interactive shell.
type SpawnOptions struct {
	// WorkDir is the preserved worktree the shell opens in.
	WorkDir string
	// Env entries appended to the inherited environment.
	Env []string
	// Cols/Rows are the initial terminal size (0 → 80x24).
	Cols, Rows uint16
}

// Session is one live shell attached to a PTY.
type Session struct {
	PTY *os.File
	Cmd *exec.Cmd
}

// Spawn starts `$SHELL -l` (bash fallback) inside opts.WorkDir on a
// fresh PTY. pty.Start runs the shell with Setsid (mandatory for a
// controlling terminal), which ALSO makes it its own process-group
// leader — do NOT add Setpgid on top, the two are mutually exclusive
// and the combination fails fork/exec with EPERM. Terminate kills the
// whole job through that session's group (a `sleep 999 &` left behind
// must not outlive the socket).
func Spawn(opts SpawnOptions) (*Session, error) {
	if opts.WorkDir == "" {
		return nil, fmt.Errorf("runshell: WorkDir is required")
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	cmd := exec.Command(shell, "-l")
	cmd.Dir = opts.WorkDir
	cmd.Env = append(os.Environ(),
		append([]string{"TERM=xterm-256color", "COLORTERM=truecolor"}, opts.Env...)...)

	cols, rows := opts.Cols, opts.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	ptyFile, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, fmt.Errorf("runshell: start %s: %w", shell, err)
	}
	return &Session{PTY: ptyFile, Cmd: cmd}, nil
}

// Resize applies a client-side window resize to the PTY.
func (s *Session) Resize(cols, rows uint16) error {
	return pty.Setsize(s.PTY, &pty.Winsize{Cols: cols, Rows: rows})
}

// Terminate tears the whole job down: SIGTERM to the process group,
// then PTY close (which delivers SIGHUP to stragglers). Idempotent.
func (s *Session) Terminate() {
	if s == nil {
		return
	}
	if s.Cmd != nil && s.Cmd.Process != nil {
		// Negative pid = the process group DetachProcessGroup created.
		_ = syscall.Kill(-s.Cmd.Process.Pid, syscall.SIGTERM)
	}
	if s.PTY != nil {
		_ = s.PTY.Close()
	}
	if s.Cmd != nil {
		// Reap; the SIGTERM (or the PTY hangup) ends the shell.
		_ = s.Cmd.Wait()
	}
}

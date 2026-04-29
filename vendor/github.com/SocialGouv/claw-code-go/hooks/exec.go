package hooks

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// HookAbortSignal allows cancelling running hooks.
type HookAbortSignal struct {
	aborted atomic.Bool
}

// NewHookAbortSignal creates a new abort signal.
func NewHookAbortSignal() *HookAbortSignal {
	return &HookAbortSignal{}
}

// Abort sets the abort flag.
func (s *HookAbortSignal) Abort() {
	s.aborted.Store(true)
}

// IsAborted returns true if the signal has been aborted.
func (s *HookAbortSignal) IsAborted() bool {
	return s.aborted.Load()
}

// commandExecution is the result of running a single shell command.
type commandExecution struct {
	Stdout        string
	Stderr        string
	ExitCode      int
	Cancelled     bool
	FailedToStart bool // true when cmd.Start() itself fails (process never ran)
}

// runShellCommand executes a command string with env vars and stdin payload.
// Polls every 20ms checking abort signal.
// Returns commandExecution with exit code, stdout, stderr.
func runShellCommand(command string, env map[string]string, stdin []byte, abort *HookAbortSignal) commandExecution {
	shell, args := shellArgs(command)
	cmd := exec.Command(shell, args...)

	// Inherit parent process environment, then add hook-specific vars.
	// Rust's Command::env() adds to the inherited env; in Go, setting
	// cmd.Env to a non-nil slice replaces it, so we must start from
	// os.Environ() to preserve PATH, HOME, SHELL, etc.
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	// Provide stdin.
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return commandExecution{
			ExitCode:      1,
			Stderr:        strings.TrimSpace(err.Error()),
			FailedToStart: true,
		}
	}

	// Wait in a goroutine so we can poll the abort signal.
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			exitCode := 0
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					exitCode = exitErr.ExitCode()
				} else {
					exitCode = 1
				}
			}
			return commandExecution{
				Stdout:   strings.TrimSpace(stdout.String()),
				Stderr:   strings.TrimSpace(stderr.String()),
				ExitCode: exitCode,
			}
		case <-ticker.C:
			if abort != nil && abort.IsAborted() {
				_ = cmd.Process.Kill()
				<-done // wait for goroutine to finish
				return commandExecution{
					Stdout:    strings.TrimSpace(stdout.String()),
					Stderr:    strings.TrimSpace(stderr.String()),
					ExitCode:  -1,
					Cancelled: true,
				}
			}
		}
	}
}

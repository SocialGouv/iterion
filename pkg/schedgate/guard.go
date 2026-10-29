package schedgate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// TailCap bounds the stdout/stderr tails stored on audit records. The
// tail (not the head) is preserved: shell errors print last.
const TailCap = 16 * 1024

// GuardKind classifies a guard execution outcome.
type GuardKind int

const (
	// GuardOK: exit 0 — the run fires and Stdout becomes vars[guard_var].
	GuardOK GuardKind = iota
	// GuardBlocked: exit non-zero — the guard deliberately said "nothing
	// to do"; the tick is skipped.
	GuardBlocked
	// GuardError: the guard itself broke — spawn failure or timeout.
	// Distinct from GuardBlocked so an operator can tell "guard said no"
	// from "guard is broken".
	GuardError
)

func (k GuardKind) String() string {
	switch k {
	case GuardOK:
		return "ok"
	case GuardBlocked:
		return "blocked"
	default:
		return "error"
	}
}

// GuardSpec is the resolved input to RunGuard.
type GuardSpec struct {
	// Command is the sh -lc snippet.
	Command string
	// Dir is the working directory (the schedule's workdir, so `gh` /
	// `git` run in repo context).
	Dir string
	// Env entries are appended to the inherited os.Environ.
	Env []string
	// Timeout bounds the subprocess; <= 0 means DefaultGuardTimeout.
	Timeout time.Duration
}

// GuardResult is what the audit and the launch layer consume.
type GuardResult struct {
	Kind GuardKind
	// ExitCode is the guard's exit status for GuardBlocked; -1 for
	// GuardError (no meaningful status).
	ExitCode int
	// Stdout is the full stdout on GuardOK (it may be structured input,
	// e.g. a JSON issue list — never truncated); the TruncTail'd tail
	// otherwise.
	Stdout string
	// StderrTail is always the TruncTail'd stderr.
	StderrTail string
	// Err is nil on GuardOK.
	Err error
	// Duration is the wall-clock guard runtime.
	Duration time.Duration
}

// guardDrainBound is this site's default bound on the wait for a guard's
// output once `sh` itself has gone, overridable through [proc.WaitDelayEnv].
const guardDrainBound = 2 * time.Second

// RunGuard executes spec.Command with `sh -lc` under its OWN
// timeout-bounded context derived from parentCtx. It never installs a
// deadline on parentCtx itself: the run has not started yet, and a
// guard timeout must not cascade into the caller's tick loop.
//
// cmd.WaitDelay bounds the orphan-pipe wait when the timeout kills `sh`
// but a grandchild keeps the inherited pipes open (same discipline as
// dispatcher hooks).
func RunGuard(parentCtx context.Context, spec GuardSpec) GuardResult {
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultGuardTimeout
	}
	cctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	if spec.Command == "" {
		return GuardResult{Kind: GuardError, ExitCode: -1, Err: fmt.Errorf("schedgate: empty guard command")}
	}

	cmd := exec.CommandContext(cctx, "sh", "-lc", spec.Command)
	// The timeout must end the guard's work, not just iterion's wait: a
	// guard that backgrounds a probe would otherwise keep running against
	// the repo after the tick moved on. WaitDelay stays as the final
	// unblock for anything that escapes the group; proc.WaitDelayEnv is the
	// operator's override of it.
	proc.TerminateGroupOnCancel(cmd)
	drainBound := proc.SetWaitDelay(cmd, guardDrainBound)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), spec.Env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)

	if proc.DrainCutShort(err) {
		// A guard's verdict is its exit code, and this one exited 0: what
		// held the pipes is a probe it left running, which the group kill
		// above has already been asked to end. Blocking the tick on it
		// would refuse a run the guard allowed, on a fact about iterion's
		// wait rather than about the repository. The tails keep whatever
		// was copied before the cut.
		err = nil
		// Stderr, not the guard's own tail: that tail is a record field a
		// reader of the tick parses, and this note is iterion's, not the
		// guard's.
		fmt.Fprintf(os.Stderr, "iterion: the schedgate guard exited, and a process it left kept its output open: stopped waiting %s\n", proc.WaitDelayHint(drainBound))
	}
	if err == nil {
		return GuardResult{
			Kind:       GuardOK,
			Stdout:     stdout.String(),
			StderrTail: TruncTail(stderr.String(), TailCap),
			Duration:   dur,
		}
	}

	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return GuardResult{
			Kind:       GuardError,
			ExitCode:   -1,
			Stdout:     TruncTail(stdout.String(), TailCap),
			StderrTail: TruncTail(stderr.String(), TailCap),
			Err:        fmt.Errorf("schedgate: guard timeout after %s: %w", timeout, context.DeadlineExceeded),
			Duration:   dur,
		}
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return GuardResult{
			Kind:       GuardBlocked,
			ExitCode:   exitErr.ExitCode(),
			Stdout:     TruncTail(stdout.String(), TailCap),
			StderrTail: TruncTail(stderr.String(), TailCap),
			Err:        fmt.Errorf("schedgate: guard exited %d", exitErr.ExitCode()),
			Duration:   dur,
		}
	}

	return GuardResult{
		Kind:       GuardError,
		ExitCode:   -1,
		Stdout:     TruncTail(stdout.String(), TailCap),
		StderrTail: TruncTail(stderr.String(), TailCap),
		Err:        fmt.Errorf("schedgate: guard exec: %w", err),
		Duration:   dur,
	}
}

// TruncTail returns s unchanged when it fits max bytes; otherwise the
// last max bytes prefixed with a truncation marker. The tail is kept
// because errors print last.
func TruncTail(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := len(s) - max
	return fmt.Sprintf("…[truncated %d bytes]…\n", cut) + s[cut:]
}

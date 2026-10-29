package proc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// WaitDelayEnv is the ONE override for every drain bound iterion puts on a
// child process — the single greppable name an operator reaches for when a
// descendant of a finished command holds its output pipes longer than the
// call site guessed.
//
// A Go duration ("5s", "2m", "1h") replaces the bound at every site; each
// site keeps its own default when the variable is unset, because a 500 ms
// MCP startup probe and a 10 s git fetch do not want the same number. "0"
// (or "off"/"no"/"none"/"unbounded") removes the bound: the escape hatch for
// an operator who would rather wait than lose the tail of a drain, loud on
// stderr because an unbounded [exec.Cmd.Wait] can park a run for good.
const WaitDelayEnv = "ITERION_PROC_WAIT_DELAY"

// SetWaitDelay bounds the wait for cmd's output pipes once the command
// itself is gone, and returns the bound it applied so the caller can NAME it
// in a log or an error — a message that quotes its own default lies as soon
// as an operator overrides it.
//
// [exec.Cmd.Wait] waits for the goroutines copying the child's stdout and
// stderr, and a copier ends only when EVERY holder of the pipe has closed
// it: a descendant the child left behind (a shell's background job, a
// credential plugin, an agent's language server) keeps Wait blocked after
// the child is reaped, with no bound of its own.
// [exec.Cmd.WaitDelay] is that bound.
//
// Call it BEFORE [exec.Cmd.Start]: Start reads the field to decide whether
// to arm the context watcher, and hands it to the watcher goroutine it
// starts, so a later assignment both races that goroutine and arms nothing
// for a Cmd that carries no Cancel.
//
// It OVERRIDES whatever cmd already carries. A call site that must defer to
// a value another layer chose (a sandbox driver's own bound) keeps its
// `cmd.WaitDelay == 0` test around the call; one that owns the bound — the
// launcher's promise that this dispatch hands the node back — calls it
// unconditionally.
func SetWaitDelay(cmd *exec.Cmd, def time.Duration) time.Duration {
	d := resolveWaitDelay(os.Getenv(WaitDelayEnv), def, &waitDelayWarnOnce, os.Stderr)
	cmd.WaitDelay = d
	return d
}

// waitDelayWarnOnce holds the override's complaint to one stderr line per
// process (the ITERION_BUDGET_EXIT_GRACE convention): an operator input
// silently replaced by the default is an override that never took, with
// nothing saying so.
var waitDelayWarnOnce sync.Once

// resolveWaitDelay is the whole decision, taking its environment, its
// warn-once and its sink as arguments so one test can exercise every branch
// — including the warning's text — without reaching into a package global.
//
// The value is read at the point of use, never memoised: a resolver frozen
// at init would make the knob untestable and would ignore an operator who
// exports it between two commands of the same process.
func resolveWaitDelay(raw string, def time.Duration, warnOnce *sync.Once, warn io.Writer) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	unbound := func() time.Duration {
		warnOnce.Do(func() {
			fmt.Fprintf(warn,
				"iterion: %s=%q leaves the drain of a finished child's output UNBOUNDED — a descendant holding its pipes now parks the wait for as long as it lives\n",
				WaitDelayEnv, raw)
		})
		return 0
	}
	switch strings.ToLower(raw) {
	case "off", "no", "none", "unbounded":
		return unbound()
	}
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil || d < 0:
		// Keep the site's default rather than invent a bound: a malformed
		// value says what the operator wanted (a different bound), not
		// which one, and a negative duration fires the timer at once —
		// cutting every drain instead of widening it.
		warnOnce.Do(func() {
			fmt.Fprintf(warn,
				"iterion: %s=%q is not a non-negative Go duration (use e.g. 500ms, 10s, 1h, or 0 for no bound) — using this site's default %s\n",
				WaitDelayEnv, raw, def)
		})
		return def
	case d == 0:
		return unbound()
	}
	return d
}

// WaitDelayHint is the trailing clause every site adds when it reports a
// drain the bound cut short: the bound that RAN, and the lever. One wording
// and one spelling of the variable, so the reports an operator can meet say
// the same thing — and so a package whose guards classify every environment
// name it spells (the claw crossing's) does not have to spell this one to
// name it.
//
// "raise it", not "widen the default": the clause is true whatever the
// operator already set, including a bound they tightened themselves.
func WaitDelayHint(applied time.Duration) string {
	return fmt.Sprintf("after %s (raise %s to wait longer)", applied, WaitDelayEnv)
}

// DrainCutShort reports whether err is the drain bound expiring and nothing
// else: the command RAN, exited successfully, and only the copy of its
// output was abandoned.
//
// [os/exec] returns [exec.ErrWaitDelay] from Wait only when the process
// exited with a successful status and nothing else failed — a non-zero exit,
// a cancellation or a copier error is reported instead and wins. So a caller
// whose verdict is the command's exit status (a hook, a guard, a syntax
// check) treats this as success without ever hiding a failure, and says so;
// a caller whose answer is the BYTES (a parsed `git` output, a sweep report)
// must keep it an error — the output it holds may be truncated — and name
// [WaitDelayEnv] so the operator can widen the bound instead of guessing.
//
// One reader for the sentinel, so the two readings stay a deliberate choice
// per call site instead of nine spellings of errors.Is.
func DrainCutShort(err error) bool { return errors.Is(err, exec.ErrWaitDelay) }

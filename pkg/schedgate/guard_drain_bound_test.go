package schedgate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// pipeHoldingGuard is a guard command that PASSES while leaving a descendant
// holding the pipes it inherited — a guard that backgrounds a probe. The
// guard's `sh` exits 0 at once; the descendant lives until release, so the
// drain bound is what ends iterion's wait and not the descendant's own exit.
//
// The holder polls a sentinel rather than sleeping, and the test joins it, so
// no shell outlives the suite. `trap ” PIPE` keeps the descendant alive
// after the bound closes the pipes under it: its late write earns EPIPE, and
// the default SIGPIPE would kill it before it could report that it was done.
type pipeHoldingGuard struct {
	t       *testing.T
	hold    string
	done    string
	command string
	freed   bool
}

func newPipeHoldingGuard(t *testing.T, dir string) *pipeHoldingGuard {
	t.Helper()
	g := &pipeHoldingGuard{t: t, hold: filepath.Join(dir, "hold"), done: filepath.Join(dir, "holder-exited")}
	if err := os.WriteFile(g.hold, []byte("held"), 0o600); err != nil {
		t.Fatalf("arm the pipe holder: %v", err)
	}
	g.command = fmt.Sprintf(
		"( trap '' PIPE; while [ -e %q ]; do sleep 0.05; done; printf 'the probe had the last word\\n' 2>/dev/null; : > %q ) &\nprintf 'the guard ran\\n'\nexit 0",
		g.hold, g.done)
	t.Cleanup(func() { g.release() })
	return g
}

func (g *pipeHoldingGuard) release() {
	g.t.Helper()
	if g.freed {
		return
	}
	g.freed = true
	if err := os.Remove(g.hold); err != nil && !os.IsNotExist(err) {
		g.t.Fatalf("release the pipe holder: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(g.done); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	g.t.Fatalf("the pipe holder did not exit after its sentinel went away — it would outlive the suite")
}

func (g *pipeHoldingGuard) stillHolding() bool {
	_, err := os.Stat(g.done)
	return os.IsNotExist(err)
}

// A guard's verdict is its EXIT CODE: `exit 0` allows the tick, non-zero
// blocks it. The drain bound expiring on a guard that exited 0 therefore
// cannot be a GuardError — that refuses a run the guard allowed, on a fact
// about iterion's wait rather than about the repository.
//
// Mutation seen red: replace the proc.DrainCutShort(err) arm in guard.go with
// `if false` — the result comes back GuardError with the sentinel in Err
// (M11).
func TestRunGuard_ACutDrainIsNotTheGuardsVerdict(t *testing.T) {
	scrubHome(t)
	t.Setenv(proc.WaitDelayEnv, "150ms")
	dir := t.TempDir()
	holder := newPipeHoldingGuard(t, dir)

	res := RunGuard(context.Background(), GuardSpec{Command: holder.command, Dir: dir})

	if !holder.stillHolding() {
		t.Fatalf("the descendant was gone before the bound: this run was not the held shape the witness needs")
	}
	if res.Kind != GuardOK {
		t.Fatalf("Kind = %v (err=%v) — a guard that exited 0 was turned into a failure by the drain of a probe it left", res.Kind, res.Err)
	}
	if res.Err != nil {
		t.Errorf("Err = %v, want nil: the guard passed", res.Err)
	}
	if !strings.Contains(res.Stdout, "the guard ran") {
		t.Errorf("Stdout = %q — what the guard said was lost with the cut drain", res.Stdout)
	}
	// The tail is the guard's own words and nothing else: the fact about
	// iterion's wait goes to stderr, because a reader of the tick parses
	// this field.
	if strings.Contains(res.StderrTail, proc.WaitDelayEnv) {
		t.Errorf("StderrTail carries iterion's own note: %q", res.StderrTail)
	}
}

// The operator's bound is the one that runs at this site, not the 2 s
// compiled in — proven by tightening it, so the assertion cannot be
// satisfied by the literal.
//
// Mutation seen red: `cmd.WaitDelay = guardDrainBound` instead of
// proc.SetWaitDelay — the guard comes back after the compiled 2 s (M12).
func TestRunGuard_TheOperatorsBoundIsTheOneThatRuns(t *testing.T) {
	scrubHome(t)
	t.Setenv(proc.WaitDelayEnv, "150ms")
	dir := t.TempDir()
	holder := newPipeHoldingGuard(t, dir)

	start := time.Now()
	res := RunGuard(context.Background(), GuardSpec{Command: holder.command, Dir: dir})
	elapsed := time.Since(start)

	if res.Kind != GuardOK {
		t.Fatalf("Kind = %v (err=%v), want GuardOK", res.Kind, res.Err)
	}
	if !holder.stillHolding() {
		t.Fatalf("the descendant was gone before the bound: nothing bounded this wait")
	}
	if elapsed >= guardDrainBound/2 {
		t.Fatalf("the guard came back after %s with %s=150ms set — this site's compiled %s bound is what ran",
			elapsed, proc.WaitDelayEnv, guardDrainBound)
	}
	if res.Duration <= 0 {
		t.Errorf("Duration = %s, want the wall clock of the bounded run", res.Duration)
	}
}

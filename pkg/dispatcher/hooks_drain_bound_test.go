package dispatcher

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// pipeHoldingHook is a hook that SUCCEEDS while leaving a descendant holding
// the stdout/stderr pipes it inherited — a hook that starts a watcher, a dev
// server, anything with `&`. The hook exits 0 at once; the descendant lives
// until [pipeHolder.release], so a drain bound shorter than the test is what
// ends iterion's wait and not the descendant's own exit.
//
// The holder polls a sentinel instead of sleeping: this package runs under
// proctest.NoProcessLeaks, so a test that leaves a `sleep` behind fails the
// whole suite — and the join below is also what proves the holder was ALIVE
// while the drain was abandoned.
type pipeHolder struct {
	t     *testing.T
	hold  string
	done  string
	hook  *Hook
	freed bool
}

func newPipeHoldingHook(t *testing.T, dir string) *pipeHolder {
	t.Helper()
	h := &pipeHolder{t: t, hold: filepath.Join(dir, "hold"), done: filepath.Join(dir, "holder-exited")}
	if err := os.WriteFile(h.hold, []byte("held"), 0o600); err != nil {
		t.Fatalf("arm the pipe holder: %v", err)
	}
	// `trap '' PIPE` is what makes the join below reliable: once the bound
	// has closed the pipes, the descendant's write earns EPIPE, and the
	// default SIGPIPE would kill it before it could say it was done.
	h.hook = &Hook{Script: fmt.Sprintf(
		"( trap '' PIPE; while [ -e %q ]; do sleep 0.05; done; printf 'the descendant had the last word\\n' 2>/dev/null; : > %q ) &\nprintf 'the hook ran\\n'\nexit 0",
		h.hold, h.done)}
	t.Cleanup(func() { h.release() })
	return h
}

// release lets the descendant go and JOINS it: the suite's leak guard counts
// anything still alive 500 ms after the last cleanup, and a holder nobody
// waited for is exactly that.
func (h *pipeHolder) release() {
	h.t.Helper()
	if h.freed {
		return
	}
	h.freed = true
	if err := os.Remove(h.hold); err != nil && !os.IsNotExist(err) {
		h.t.Fatalf("release the pipe holder: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(h.done); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("the pipe holder did not exit after its sentinel went away — it would outlive the suite")
}

// stillHolding reports whether the descendant is alive, which is the
// precondition of both witnesses below: a drain the holder had already let go
// of proves nothing about the bound.
func (h *pipeHolder) stillHolding() bool {
	_, err := os.Stat(h.done)
	return os.IsNotExist(err)
}

// A hook's verdict is its exit status. When the bound abandons the drain of a
// hook that exited 0, the lifecycle step it guards must still proceed: before
// #2469 cmd.Run's exec.ErrWaitDelay came back as "hook X: … (after …)" and
// failed the step — a dispatcher refusing a hook that had succeeded.
//
// Mutation seen red: replace the proc.DrainCutShort(err) arm in hooks.go with
// `if false` — Run returns the sentinel as the hook's failure (M9).
func TestRunHook_ADrainCutShortIsTheHooksSuccess(t *testing.T) {
	t.Setenv(proc.WaitDelayEnv, "150ms")
	dir := t.TempDir()
	holder := newPipeHoldingHook(t, dir)
	var logs bytes.Buffer

	err := holder.hook.Run(context.Background(), iterlog.New(iterlog.LevelInfo, &logs), "holding", dir, nil)

	if !holder.stillHolding() {
		t.Fatalf("the descendant was gone before the bound: this run was not the held shape the witness needs")
	}
	if err != nil {
		t.Fatalf("a hook that exited 0 was failed by the drain of a process it left: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "kept its output open") || !strings.Contains(out, proc.WaitDelayEnv) {
		t.Errorf("the abandoned drain is unreported, or does not name the knob that widens it: %q", out)
	}
	// What the hook DID say is still in the log: tolerating the cut is not
	// dropping the output already copied.
	if !strings.Contains(out, "the hook ran") {
		t.Errorf("the hook's own output was lost with the cut drain: %q", out)
	}
	// And the descendant's last word is NOT there: the bound did cut a live
	// drain, so the tolerance above is not vacuous.
	if strings.Contains(out, "the descendant had the last word") {
		t.Errorf("the drain was never cut — the witness proves nothing: %q", out)
	}
}

// The operator's bound is the one that runs at this site, not the 2 s
// compiled in. Proven by TIGHTENING it: a 150 ms bound returns while the
// site's own default would still be waiting, so the assertion reads the
// applied value and cannot be satisfied by the literal. (The widening
// direction — the point of the knob — is pinned end to end on the helper
// itself and on the claw crossing.)
//
// Mutation seen red: `cmd.WaitDelay = hookDrainBound` instead of
// proc.SetWaitDelay — the hook comes back after the compiled 2 s (M10).
func TestRunHook_TheOperatorsBoundIsTheOneThatRuns(t *testing.T) {
	t.Setenv(proc.WaitDelayEnv, "150ms")
	dir := t.TempDir()
	holder := newPipeHoldingHook(t, dir)
	var logs bytes.Buffer

	start := time.Now()
	err := holder.hook.Run(context.Background(), iterlog.New(iterlog.LevelInfo, &logs), "holding", dir, nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !holder.stillHolding() {
		t.Fatalf("the descendant was gone before the bound: nothing bounded this wait")
	}
	// Half of the site's compiled default: inside it, the operator's bound
	// ran; past it, the literal did.
	if elapsed >= hookDrainBound/2 {
		t.Fatalf("the hook came back after %s with %s=150ms set — this site's compiled %s bound is what ran",
			elapsed, proc.WaitDelayEnv, hookDrainBound)
	}
	if out := logs.String(); !strings.Contains(out, "150ms") {
		t.Errorf("the report names a bound that did not run: %q", out)
	}
}

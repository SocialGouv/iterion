package proc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// What the drain bound can and cannot reach, measured rather than assumed:
// [SetWaitDelay] is the one owner of the class, and the next site added to it
// is chosen from this answer. A bound put on a Cmd for which exec started no
// copier is inert — Wait has nothing to await — and an inert bound is worse
// than none: it reads, in the code and in the operator's documentation, like
// a promise the site keeps.
//
// The condition is the COPIER, not the direction: the third arm owns both
// output pipes, the shape the first two tell apart, and a copied STDIN alone
// puts it back in reach.
//
// One shape, three arms, one leftover holder where a holder is the point: the
// child prints its answer, leaves a process holding the output and exits 0.
// The wait is MEASURED against a deadline, so a regression fails in seconds
// instead of holding the package to its timeout.
//
// Mutations seen red: drop the `cmd.WaitDelay = d` assignment in
// [SetWaitDelay] — the copied arm reads Wait = <nil> where the cut belongs;
// and make [DrainCutShort] answer for a nil error — the owned-pipe arm reads
// a cut on a Cmd with no drain to cut. Both arms' claims are platform facts,
// so each is also seen red by a fixture mutation: giving the owned-pipe arm
// a copier (`cmd.Stderr = &copied` beside the pipe it owns) makes Wait answer
// ErrWaitDelay, and taking the copied stdin off the third arm makes its Wait
// answer nil.
func TestTheDrainBoundReachesACopiedOutputAndNotAPipeItsCallerOwns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture, and the holder is a background job")
	}
	// The bound asserted below is this test's, so the environment the suite
	// runs in does not get to choose it.
	t.Setenv(WaitDelayEnv, "")
	const bound = 300 * time.Millisecond
	const answer = "the-childs-answer"

	// holderScript prints answer, leaves a process holding the output past
	// the bound, and exits 0. `trap '' PIPE` keeps the holder alive once the
	// bound closes the pipes under it, so it can report its own exit and no
	// shell outlives the suite.
	holderScript := func(done string) string {
		return fmt.Sprintf("( trap '' PIPE; sleep 1; : > %q ) &\nprintf '%%s\\n' %q\nexit 0", done, answer)
	}

	t.Run("a copied output is bounded, and says so", func(t *testing.T) {
		done := filepath.Join(t.TempDir(), "holder-exited")
		cmd := exec.Command("sh", "-c", holderScript(done))
		var out bytes.Buffer
		// A writer that is not an *os.File: exec opens a pipe and copies it
		// in a goroutine, which is what Wait waits for past the exit.
		cmd.Stdout = &out
		applied := SetWaitDelay(cmd, bound)
		if applied != bound {
			t.Fatalf("SetWaitDelay applied %s, want this test's %s", applied, bound)
		}
		t.Cleanup(func() { awaitLeftoverHolder(t, done) })

		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		err := waitWithin(t, cmd, 30*time.Second)
		if !DrainCutShort(err) {
			t.Fatalf("Wait = %v, want the drain bound cutting the copy short (ErrWaitDelay)", err)
		}
		// The bytes already copied are kept — the cut is of the tail, not of
		// the answer — which is why a site whose verdict is the exit status
		// can report the cut and keep the success.
		if got := strings.TrimSpace(out.String()); got != answer {
			t.Errorf("stdout = %q, want the answer the child printed before the cut (%q)", got, answer)
		}
	})

	t.Run("a pipe its caller owns is not bounded, and no cut is reported", func(t *testing.T) {
		done := filepath.Join(t.TempDir(), "holder-exited")
		cmd := exec.Command("sh", "-c", holderScript(done))
		// The caller owns the pipe: exec records the read end and starts NO
		// copier, so there is nothing for the bound to stop waiting for.
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("stdout pipe: %v", err)
		}
		if applied := SetWaitDelay(cmd, bound); applied != bound {
			t.Fatalf("SetWaitDelay applied %s, want this test's %s", applied, bound)
		}
		t.Cleanup(func() { awaitLeftoverHolder(t, done) })

		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		// Read the one line the child printed, and no further: Wait closes
		// this end, so reading to EOF would wait out the holder instead of
		// the bound — the caller's own drain is the caller's problem, which
		// is the point.
		line, readErr := bufio.NewReader(pipe).ReadString('\n')
		if readErr != nil {
			t.Fatalf("read the answer: %v", readErr)
		}
		if got := strings.TrimSpace(line); got != answer {
			t.Fatalf("read %q, want %q", got, answer)
		}
		waitErr := waitWithin(t, cmd, 30*time.Second)
		if waitErr != nil {
			t.Fatalf("Wait = %v, want nil: a Cmd whose pipes its caller owns has no copier to abandon", waitErr)
		}
		if DrainCutShort(waitErr) {
			t.Error("Wait reported the drain cut short on a Cmd with no drain to cut")
		}
	})

	t.Run("a copied stdin is a copier too, so owned output pipes are not out of reach", func(t *testing.T) {
		// The counterexample to "the caller owns its pipes, so nothing is
		// copied": exec starts a copier for ANY stream that is not an
		// *os.File, and Wait awaits every copier it started. A driver that
		// streams a script in on stdin (strings.NewReader, the docker and
		// kubernetes ones) gives a Cmd whose output pipes its caller owns a
		// copier all the same — which is why the rule is read by the copier
		// and not by the direction.
		const reads = 5 * bound
		cmd := exec.Command("sh", "-c", fmt.Sprintf("printf '%%s\\n' %q; exit 0", answer))
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("stdout pipe: %v", err)
		}
		if _, err := cmd.StderrPipe(); err != nil {
			t.Fatalf("stderr pipe: %v", err)
		}
		cmd.Stdin = slowStdin{reads: reads}
		if applied := SetWaitDelay(cmd, bound); applied != bound {
			t.Fatalf("SetWaitDelay applied %s, want this test's %s", applied, bound)
		}

		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		line, readErr := bufio.NewReader(out).ReadString('\n')
		if readErr != nil {
			t.Fatalf("read the answer: %v", readErr)
		}
		if got := strings.TrimSpace(line); got != answer {
			t.Fatalf("read %q, want %q", got, answer)
		}
		start := time.Now()
		waitErr := waitWithin(t, cmd, 30*time.Second)
		if !DrainCutShort(waitErr) {
			t.Fatalf("Wait = %v, want the drain bound cutting short the stdin copy exec started", waitErr)
		}
		// And the bound is soft: closing the descriptors exec owns releases
		// a copier blocked writing into a holder's pipe, never one blocked
		// reading from a reader this caller owns, so the answer comes at the
		// reader's end rather than at the bound.
		if waited := time.Since(start); waited < reads-bound {
			t.Errorf("Wait came back in %s, before the reader it copies let go (%s): the bound is reported as a hard one", waited, reads)
		}
	})
}

// slowStdin is a stdin reader that is not an *os.File and is still reading
// when the bound fires: exec copies it in a goroutine, and the goroutine is
// what Wait awaits.
type slowStdin struct{ reads time.Duration }

func (s slowStdin) Read([]byte) (int, error) {
	time.Sleep(s.reads)
	return 0, io.EOF
}

// waitWithin waits for cmd and fails if the wait is not over within d: a
// bound that did not fire parks the caller, and a test that waited it out
// would report the package's timeout instead of the defect.
func waitWithin(t *testing.T, cmd *exec.Cmd, d time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		t.Fatalf("Wait did not return within %s: the wait is unbounded while a process the child left holds its output", d)
		return nil
	}
}

// awaitLeftoverHolder joins the process the child left behind: it writes done
// as its last act, and a test that returned without it would leave a shell
// running past the suite.
func awaitLeftoverHolder(t *testing.T, done string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(done); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("the leftover holder never reported its exit (%s) — it may outlive the suite", done)
}

// The sentinel the two arms are told apart by is exec's own, so a reader of
// DrainCutShort never has to wonder which error a site is matching.
func TestDrainCutShortIsExecsOwnSentinelAndNothingElse(t *testing.T) {
	t.Parallel()
	if !DrainCutShort(fmt.Errorf("wrapped: %w", exec.ErrWaitDelay)) {
		t.Error("a wrapped ErrWaitDelay is not read as the drain cut short")
	}
	if DrainCutShort(errors.New("exit status 1")) {
		t.Error("an unrelated error is read as the drain cut short")
	}
	if DrainCutShort(nil) {
		t.Error("nil is read as the drain cut short")
	}
}

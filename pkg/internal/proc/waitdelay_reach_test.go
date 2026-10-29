package proc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
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
// is chosen from this answer. A bound put on a Cmd whose caller owns its
// pipes is inert — exec starts no copier for an owned pipe, so Wait has
// nothing to await — and an inert bound is worse than none: it reads, in the
// code and in the operator's documentation, like a promise the site keeps.
//
// One shape, both arms, one leftover holder: the child prints its answer,
// leaves a process holding the output and exits 0. The wait is MEASURED
// against a deadline, so a regression fails in seconds instead of holding
// the package to its timeout.
//
// Mutations seen red: drop the `cmd.WaitDelay = d` assignment in
// [SetWaitDelay] — the copied arm reads Wait = <nil> where the cut belongs;
// and make [DrainCutShort] answer for a nil error — the owned-pipe arm reads
// a cut on a Cmd with no drain to cut. The owned-pipe arm's own claim is a
// platform fact, so it is also seen red by giving that arm a copier
// (`cmd.Stderr = &copied` beside the pipe it owns): Wait then answers
// ErrWaitDelay, which is exactly the shape this test tells apart.
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

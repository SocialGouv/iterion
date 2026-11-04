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
	"sync"
	"testing"
	"testing/synctest"
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
// One shape, three arms: the child prints its answer and exits 0 while
// something it leaves behind still holds a stream exec copies — a process
// holding the output ([leftoverHolder]), or the reader exec copies to stdin.
// That holder lets go only when the TEST says so, after the verdict, so the
// bound is the one thing that can end a copy before it: no race between a
// sleep and the bound decides an arm on a loaded machine. A wait the bound
// does not end is measured against a ceiling of a hundred bounds, so a
// regression fails readably instead of holding the package to its timeout.
//
// Mutations seen red: drop the `cmd.WaitDelay = d` assignment in
// [SetWaitDelay] — the copied-output arm reads no answer within the ceiling
// where the cut belongs, and the copied-stdin arm reads Wait = <nil>; and make
// [DrainCutShort] answer for a nil error — the owned-pipe arm reads a cut on a
// Cmd with no drain to cut. Both arms' claims are platform facts, so each is
// also seen red by a fixture mutation: giving the owned-pipe arm a copier
// (`cmd.Stderr = &copied` beside the pipe it owns) makes Wait answer
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
	// A ceiling, not an expectation: a wait still unanswered a hundred bounds
	// in is a bound that never fired, not a slow machine.
	const ceiling = 100 * bound
	const answer = "the-childs-answer"
	printAnswer := fmt.Sprintf("printf '%%s\\n' %q\nexit 0", answer)

	t.Run("a copied output is bounded, and says so", func(t *testing.T) {
		h := newLeftoverHolder(t)
		cmd := exec.Command("sh", "-c", h.fork()+printAnswer)
		h.handTo(cmd)
		var out bytes.Buffer
		// A writer that is not an *os.File: exec opens a pipe and copies it
		// in a goroutine, which is what Wait waits for past the exit.
		cmd.Stdout = &out
		applied := SetWaitDelay(cmd, bound)
		if applied != bound {
			t.Fatalf("SetWaitDelay applied %s, want this test's %s", applied, bound)
		}

		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		err := waitWithin(t, cmd, ceiling)
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
		h := newLeftoverHolder(t)
		cmd := exec.Command("sh", "-c", h.fork()+printAnswer)
		h.handTo(cmd)
		// The caller owns the pipe: exec records the read end and starts NO
		// copier, so there is nothing for the bound to stop waiting for.
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("stdout pipe: %v", err)
		}
		if applied := SetWaitDelay(cmd, bound); applied != bound {
			t.Fatalf("SetWaitDelay applied %s, want this test's %s", applied, bound)
		}

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
		waitErr := waitWithin(t, cmd, ceiling)
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
		//
		// Inside a synctest bubble: the copier parks in a reader this test
		// owns, which is durable blocking, so once the child is reaped every
		// goroutine of the bubble is parked and its clock — not the
		// machine's — carries Wait past the bound.
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})
			cmd := exec.Command("sh", "-c", printAnswer)
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatalf("stdout pipe: %v", err)
			}
			if _, err := cmd.StderrPipe(); err != nil {
				t.Fatalf("stderr pipe: %v", err)
			}
			cmd.Stdin = heldStdin{release: release}
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
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			// The bubble's clock stands still while Wait reaps the child,
			// then jumps: the bound's timer fires before this sleep ends, and
			// synctest.Wait returns once Wait has parked again after it.
			time.Sleep(2 * bound)
			synctest.Wait()
			select {
			case err := <-done:
				if !DrainCutShort(err) {
					t.Fatalf("Wait = %v, want the drain bound cutting short the stdin copy exec started", err)
				}
				// The bound is soft: closing the descriptors exec owns
				// releases a copier blocked writing into a holder's pipe,
				// never one blocked reading from a reader this caller owns,
				// so the answer comes at the reader's end, not at the bound.
				t.Fatalf("Wait answered %v past the bound while the reader it copies still held: the bound is reported as a hard one", err)
			default:
			}
			close(release)
			if err := <-done; !DrainCutShort(err) {
				t.Fatalf("Wait = %v, want the drain bound cutting short the stdin copy exec started", err)
			}
		})
	})
}

// heldStdin is a stdin reader that is not an *os.File and returns only once
// release is closed: exec copies it in a goroutine, the goroutine is what
// Wait awaits, and the test alone decides when it lets go.
type heldStdin struct{ release <-chan struct{} }

func (s heldStdin) Read([]byte) (int, error) {
	<-s.release
	return 0, io.EOF
}

// leftoverHolder is the process a child leaves behind holding the pipes it
// inherited — a shell's background job, the shape every drain-bound site
// meets — and it holds them until the TEST lets it go, never for a time of
// its own: whatever ends a copy before that is the drain bound.
//
// The release is a pipe the child gets as fd 3: the holder blocks reading
// it, and closing the test's write end — close-on-exec, so no child holds a
// copy — is the release. The holder then runs its late commands and reports
// its exit at done as its last act. It ignores SIGPIPE, so it stays alive to
// report when the bound closed the pipe a late write goes to.
type leftoverHolder struct {
	hold, release *os.File
	done          string
	cmd           *exec.Cmd
	once          sync.Once
}

// newLeftoverHolder makes the release pipe and registers on t the release
// and the join, which run whether or not the test let go first.
func newLeftoverHolder(t *testing.T) *leftoverHolder {
	t.Helper()
	hold, release, err := os.Pipe()
	if err != nil {
		t.Fatalf("release pipe: %v", err)
	}
	h := &leftoverHolder{hold: hold, release: release, done: filepath.Join(t.TempDir(), "holder-exited")}
	t.Cleanup(func() {
		h.letGo(t)
		_ = hold.Close()
	})
	return h
}

// fork is the script line that backgrounds the holder; late runs once the
// test lets it go.
func (h *leftoverHolder) fork(late ...string) string {
	steps := append([]string{"trap '' PIPE", "read -r released <&3"}, late...)
	steps = append(steps, fmt.Sprintf(": > %q", h.done))
	return "( " + strings.Join(steps, "; ") + " ) &\n"
}

// handTo gives cmd's child the end the holder reads its release from, as
// fd 3. Before Start.
func (h *leftoverHolder) handTo(cmd *exec.Cmd) {
	cmd.ExtraFiles = []*os.File{h.hold}
	h.cmd = cmd
}

// letGo releases the holder and, if the child that forks it started, joins
// it. Once: an arm that lets go early is cleaned up by the same call.
func (h *leftoverHolder) letGo(t *testing.T) {
	t.Helper()
	h.once.Do(func() {
		if err := h.release.Close(); err != nil {
			t.Errorf("release the holder: %v", err)
		}
		if h.cmd != nil && h.cmd.Process != nil {
			awaitLeftoverHolder(t, h.done)
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

//go:build unix

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The writer refuses when it cannot tell the size it received from the size
// sent — a missing size included: a check that cannot run fails closed.
func TestWriteFileScriptFailsClosed(t *testing.T) {
	target := filepath.Join(t.TempDir(), "f")
	cmd := exec.Command("sh", "-c", writeFileScript, "sh", target, "")
	cmd.Stdin = strings.NewReader("abc")
	if err := cmd.Run(); err == nil {
		t.Fatal("the writer accepted a write it could not check")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the unchecked content was put in place: %v", err)
	}
}

// Two writers of one path never mix: the first, half way through its stream,
// is overtaken by a second that completes; the first then completes, and the
// file is wholly one of the two.
func TestWriteFileScriptWritersNeverMix(t *testing.T) {
	target := filepath.Join(t.TempDir(), "f")
	a, b := bytes.Repeat([]byte("A"), 1<<16), bytes.Repeat([]byte("B"), 1<<16)
	first := exec.Command("sh", "-c", writeFileScript, "sh", target, "65536")
	in, err := first.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Write(a[:len(a)/2]); err != nil {
		t.Fatal(err)
	}
	second := exec.Command("sh", "-c", writeFileScript, "sh", target, "65536")
	second.Stdin = bytes.NewReader(b)
	if out, err := second.CombinedOutput(); err != nil {
		t.Fatalf("the second writer failed: %v: %s", err, out)
	}
	if _, err := in.Write(a[len(a)/2:]); err != nil {
		t.Fatal(err)
	}
	in.Close()
	_ = first.Wait()
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, a) && !bytes.Equal(got, b) {
		t.Fatalf("two writers mixed: %d bytes, %d A, %d B", len(got), bytes.Count(got, []byte("A")), bytes.Count(got, []byte("B")))
	}
}

// noProofRun answers every exec with success and nothing on stdout — an exec
// client whose stream was lost before the writer spoke.
type noProofRun struct{}

func (noProofRun) Driver() string { return "no-proof" }
func (noProofRun) Command(ctx context.Context, cmd []string, _ ExecOpts) *exec.Cmd {
	return exec.CommandContext(ctx, "true")
}
func (noProofRun) Exec(_ context.Context, _ []string, opts ExecOpts) (ExecResult, error) {
	if opts.Stdin != nil {
		_, _ = io.Copy(io.Discard, opts.Stdin)
	}
	return ExecResult{}, nil
}
func (noProofRun) Cleanup(context.Context) error { return nil }

// A write whose writer never said the file is in place is not a write: the
// exec client's success alone proves nothing.
func TestWriteFileExec_AWriteWithoutItsProofFails(t *testing.T) {
	err := WriteFileExec(context.Background(), noProofRun{}, "/tmp/x", []byte("payload"))
	if err == nil || !errors.Is(err, ErrExecStatusUnknown) {
		t.Fatalf("a write without its proof: %v, want an unproven end", err)
	}
}

// A destination that is a directory is refused: mv would put the file inside
// it, and the writer would prove a write that never reached its path.
func TestWriteFileScriptRefusesADirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "creds")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", writeFileScript, "sh", dir, "10")
	cmd.Stdin = strings.NewReader("credential")
	out, err := cmd.Output()
	if err == nil || strings.Contains(string(out), writeFileDone) {
		t.Fatalf("a directory destination was written and proven: %v, %q", err, out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the refused write left %d file(s) in the directory", len(entries))
	}
}

// A signal that ends the writer takes its temp file with it.
func TestWriteFileScriptTakesItsTempFileOnTERM(t *testing.T) {
	target := filepath.Join(t.TempDir(), "f")
	cmd := exec.Command("sh", "-c", writeFileScript, "sh", target, "100")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	// The temp file exists once cat has opened it.
	for i := 0; i < 100; i++ {
		if m, _ := filepath.Glob(target + ".tmp.*"); len(m) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// The shell runs its trap once its foreground cat is done: the stream's
	// end, as a dead exec client's stream ends.
	in.Close()
	_ = cmd.Wait()
	if m, _ := filepath.Glob(target + ".tmp.*"); len(m) > 0 {
		t.Fatalf("a TERM left the writer's temp file behind: %v", m)
	}
}

// scriptedRun answers each sweep exec with the next script of steps, then
// with the real sweep program, run here.
type scriptedRun struct {
	steps    []string
	attempts int
}

func (r *scriptedRun) Driver() string { return "scripted" }
func (r *scriptedRun) Command(ctx context.Context, argv []string, _ ExecOpts) *exec.Cmd {
	r.attempts++
	if r.attempts <= len(r.steps) {
		return exec.CommandContext(ctx, "sh", "-c", r.steps[r.attempts-1])
	}
	return exec.CommandContext(ctx, argv[0], argv[1:]...)
}
func (r *scriptedRun) Exec(context.Context, []string, ExecOpts) (ExecResult, error) {
	return ExecResult{}, nil
}
func (r *scriptedRun) Cleanup(context.Context) error { return nil }

// Only an exec that never reached the container is tried again: it signalled
// nothing. One that reached it and was cut may have, and its sweep is final.
func TestSweepRetriesOnlyAnExecThatNeverReachedTheContainer(t *testing.T) {
	never := &scriptedRun{steps: []string{"echo 'Unable to connect to the server: dial tcp 10.0.0.1:443: i/o timeout' >&2; exit 1"}}
	if o := NewCompletion("t").Sweep(never, 10*time.Second); !o.Stopped() || never.attempts != 2 {
		t.Fatalf("an exec that never reached the container: attempts=%d stopped=%v (%v), want a second, settling attempt", never.attempts, o.Stopped(), o.Err)
	}
	cut := &scriptedRun{steps: []string{"echo 'error: stream reset by peer' >&2; exit 1"}}
	if o := NewCompletion("t").Sweep(cut, 10*time.Second); o.Stopped() || cut.attempts != 1 {
		t.Fatalf("an exec cut after reaching the container: attempts=%d stopped=%v, want one attempt and an unsettled end", cut.attempts, o.Stopped())
	}
}

// The whole sweep ends within its budget, however long one attempt would take.
func TestSweepEndsWithinItsBudget(t *testing.T) {
	hangs := &scriptedRun{steps: []string{"sleep 30"}}
	start := time.Now()
	o := NewCompletion("t").Sweep(hangs, time.Second)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("a sweep with a budget of 1s took %s", took)
	}
	if o.Stopped() {
		t.Fatal("a sweep cut by its budget proved the command stopped")
	}
}

// kubectl's own word for an API server that refused the connection is an
// exec that never reached the container: the sweep is tried again.
func TestSweepRetriesWhenKubectlsServerRefusedTheConnection(t *testing.T) {
	refused := &scriptedRun{steps: []string{"echo 'The connection to the server 10.0.0.1:6443 was refused - did you specify the right host or port?' >&2; exit 1"}}
	if o := NewCompletion("t").Sweep(refused, 10*time.Second); !o.Stopped() || refused.attempts != 2 {
		t.Fatalf("a refused API server: attempts=%d stopped=%v (%v), want a second, settling attempt", refused.attempts, o.Stopped(), o.Err)
	}
}

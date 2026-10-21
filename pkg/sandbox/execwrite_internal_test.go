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
	"testing"
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

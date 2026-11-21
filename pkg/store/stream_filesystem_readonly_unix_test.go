//go:build unix

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/SocialGouv/iterion/internal/testfs"
)

func TestFilesystemStreamReadonlyMissingLock(t *testing.T) {
	t.Parallel()
	s := filesystemSelectionProbeStore(t)
	if _, e := s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", WorkflowName: "fixture", Format: ExactValueFormat, DataProtocol: HistoricalRunData}); e != nil {
		t.Fatal(e)
	}
	testfs.RemoveAll(t, s.root, filepath.Join(s.root, ".run-write-locks"))
	filesystemSelectionProbeWrite(t, filepath.Join(s.runDir("r"), "run.log"), "immutable-fixture")
	lockdir := filepath.Join(s.root, ".run-write-locks")
	if _, e := os.Stat(lockdir); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("fixture already has lock domain: %v", e)
	}
	for _, path := range []string{s.root, filepath.Join(s.root, "runs"), s.runDir("r")} {
		if e := os.Chmod(path, 0500); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { os.Chmod(path, 0700) })
	}
	ctx := t.Context()
	n := filesystemSelectionProbeSelect(t, s, ctx)
	if got := filesystemSelectionProbeRead(t, n, ctx); got != "immutable-fixture" {
		t.Fatal(got)
	}
	if e := n.Validate(t.Context()); e != nil {
		t.Fatal(e)
	}
	if _, e := n.Terminal(t.Context()); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(lockdir); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("reader created stable lock namespace: %v", e)
	}
}
func TestFilesystemStreamLockErrorsCannotServeRead(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"denied", "fifo", "symlink"} {
		t.Run(shape, func(t *testing.T) {
			s := filesystemSelectionProbeStore(t)
			filesystemSelectionProbeHeader(t, s, 0, 1)
			filesystemSelectionProbeWrite(t, filepath.Join(s.runDir("r"), "run.log"), "original")
			p := filepath.Join(s.root, ".run-write-locks", "r")
			if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
				t.Fatal(e)
			}
			switch shape {
			case "denied":
				filesystemSelectionProbeWrite(t, p, "lock")
				if e := os.Chmod(p, 0000); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { os.Chmod(p, 0600) })
			case "fifo":
				if e := syscall.Mkfifo(p, 0600); e != nil {
					t.Fatal(e)
				}
			case "symlink":
				if e := os.Symlink("absent-target", p); e != nil {
					t.Fatal(e)
				}
			}
			if shape == "denied" {
				if f, e := os.Open(p); e == nil {
					f.Close()
					t.Skip("process can bypass permissions; denial oracle unavailable")
				} else if !errors.Is(e, os.ErrPermission) {
					t.Fatalf("denial fixture failed for another reason: %v", e)
				}
			}
			info, e := os.Lstat(p)
			if e != nil {
				t.Fatal(e)
			}
			if shape == "fifo" && info.Mode()&os.ModeNamedPipe == 0 {
				t.Fatal("FIFO fixture absent")
			}
			if shape == "symlink" && info.Mode()&os.ModeSymlink == 0 {
				t.Fatal("symlink fixture absent")
			}
			ctx := t.Context()
			n, observed, e := s.OpenFilesystemStream(ctx, "r", FilesystemStreamOpenOptions{})
			if n != nil {
				n.Close()
			}
			want := ErrRunDataIdentityChanged
			switch shape {
			case "denied":
				want = os.ErrPermission
			case "symlink":
				want = syscall.ELOOP
			}
			if observed || n != nil || !errors.Is(e, want) || errors.Is(e, context.DeadlineExceeded) {
				t.Fatalf("%s lock refusal: observed=%v err=%v want=%v without deadline", shape, observed, e, want)
			}
		})
	}
}

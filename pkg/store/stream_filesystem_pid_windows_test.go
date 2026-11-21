//go:build windows

package store

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func filesystemSelectionProbePublicWindows(t *testing.T) *FilesystemRunStore {
	t.Helper()
	s := filesystemSelectionProbeStore(t)
	filesystemSelectionProbeHeader(t, s, 0, 1)
	filesystemSelectionProbeWrite(t, filepath.Join(s.runDir("r"), "run.log"), "original")
	return s
}

func TestFilesystemStreamWindowsPIDStates(t *testing.T) {
	t.Parallel()
	for _, record := range []string{"0", "4294967296", "-1", "x", "12\n", "12345678901"} {
		t.Run(record, func(t *testing.T) {
			s := filesystemSelectionProbePublicWindows(t)
			p := filepath.Join(s.root, ".run-write-locks", "r")
			filesystemSelectionProbeWrite(t, p, record)
			n, ok, e := s.OpenFilesystemStream(t.Context(), "r", FilesystemStreamOpenOptions{})
			if n != nil {
				n.Close()
			}
			if n != nil || ok || !errors.Is(e, ErrInvalidRunDataIdentity) {
				t.Fatalf("record %q observed=%v n=%v error=%v", record, ok, n, e)
			}
			b, _ := os.ReadFile(p)
			if string(b) != record {
				t.Fatal("reader mutated PID")
			}
		})
	}
	for _, record := range []string{"", strconv.Itoa(os.Getpid())} {
		t.Run("pending-"+record, func(t *testing.T) {
			s := filesystemSelectionProbePublicWindows(t)
			p := filepath.Join(s.root, ".run-write-locks", "r")
			filesystemSelectionProbeWrite(t, p, record)
			filesystemSelectionProbeBusyPID(t, p)
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
			defer cancel()
			n, ok, e := s.OpenFilesystemStream(ctx, "r", FilesystemStreamOpenOptions{})
			if n != nil {
				n.Close()
			}
			if n != nil || ok || !errors.Is(e, context.DeadlineExceeded) {
				t.Fatalf("pending observed=%v err=%v", ok, e)
			}
			b, _ := os.ReadFile(p)
			if string(b) != record {
				t.Fatal("reader mutated PID")
			}
		})
	}
}
func TestFilesystemStreamWindowsDeadPID(t *testing.T) {
	t.Parallel()
	s := filesystemSelectionProbePublicWindows(t)
	cmd := exec.Command("cmd.exe", "/c", "exit", "0")
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	pid := cmd.Process.Pid
	if e := cmd.Wait(); e != nil {
		t.Fatal(e)
	}
	if pidAliveWindows(uint32(pid)) {
		t.Skip("exited process PID still alive or unobservable")
	}
	p := filepath.Join(s.root, ".run-write-locks", "r")
	record := strconv.Itoa(pid)
	filesystemSelectionProbeWrite(t, p, record)
	n := filesystemSelectionProbeSelect(t, s, t.Context())
	if got := filesystemSelectionProbeRead(t, n, t.Context()); got != "original" {
		t.Fatal(got)
	}
	b, e := os.ReadFile(p)
	if e != nil || string(b) != record {
		t.Fatalf("reader deleted stale PID: %v", e)
	}
}
func filesystemSelectionProbeBusyPID(t *testing.T, path string) {
	t.Helper()
	lock, state, err := tryStreamReadLock(path, "test writer")
	if lock != nil {
		defer lock.Unlock()
	}
	if state != streamReadLockBusy || lock != nil {
		t.Fatalf("PID premise: state=%v lock=%v error=%v, want busy without ownership", state, lock, err)
	}
}

func TestFilesystemStreamWindowsWriterUnlock(t *testing.T) {
	t.Parallel()
	s := filesystemSelectionProbePublicWindows(t)
	lock, err := s.acquireRunWriteLock("r")
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			if err := lock.Unlock(); err != nil {
				t.Errorf("cleanup writer lock: %v", err)
			}
		}
	}()
	filesystemSelectionProbeBusyPID(t, filepath.Join(s.root, ".run-write-locks", "r"))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		n, observed, err := s.OpenFilesystemStream(ctx, "r", FilesystemStreamOpenOptions{})
		if n != nil {
			defer n.Close()
		}
		if err == nil && (!observed || n == nil) {
			err = errors.New("missing observation")
		}
		done <- err
	}()
	defer func() { cancel(); <-finished }()
	select {
	case err := <-done:
		t.Fatalf("reader escaped live writer: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	released = true
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemStreamWindowsStrictRefusal(t *testing.T) {
	t.Parallel()
	if err := RequireRunDataStorage(IncarnationRunData); err != nil {
		t.Skipf("protocol-one admission disabled: %v", err)
	}
	s := filesystemSelectionProbeStore(t)
	filesystemSelectionProbeHeader(t, s, 1, 2)
	n, observed, err := s.OpenFilesystemStream(t.Context(), "r", FilesystemStreamOpenOptions{})
	if n != nil {
		n.Close()
	}
	if observed || n != nil || !errors.Is(err, ErrRunDataCapability) {
		t.Fatalf("unsupported strict platform: observed=%v error=%v", observed, err)
	}
}

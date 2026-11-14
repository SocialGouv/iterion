//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package testfs

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The mechanism, deterministically, at any GOMAXPROCS: a FIFO keeps
// WriteExecutable inside its open(O_WRONLY) until a reader arrives, then
// inside its write() for as long as the body outgrows the pipe — and for as
// long as it is in either, no fork may start: ForkLock cannot be taken for
// writing. The close is not held by a FIFO; that window stays the stress
// test's (≥ 2 Ps).
func TestWriteExecutableHoldsForksOffWhileItsWriteIsOpen(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fake")
	if err := syscall.Mkfifo(fifo, 0o700); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 1<<20+1) // more than any pipe holds: the write blocks until drained
	done := make(chan struct{})
	go func() {
		defer close(done)
		WriteExecutable(t, fifo, body)
	}()
	heldInOpen := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if !syscall.ForkLock.TryLock() {
			heldInOpen = true
			break
		}
		syscall.ForkLock.Unlock()
	}
	// Non-blocking: a helper that never opened the FIFO cannot hang the test.
	r, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	takeableInWrite := false
	for deadline := time.Now().Add(100 * time.Millisecond); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if syscall.ForkLock.TryLock() {
			syscall.ForkLock.Unlock()
			takeableInWrite = true
			break
		}
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if !heldInOpen {
		t.Fatal("a fork could start for 2s while WriteExecutable sat in its open: ForkLock is not held across the open")
	}
	if takeableInWrite {
		t.Fatal("a fork could start while WriteExecutable was parked inside write(): ForkLock is not held across the write")
	}
}

// An existing file keeps its mode through os.WriteFile; the helper's promise
// is an executable, so it sets the mode itself.
func TestWriteExecutableMakesAnExistingFileExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	WriteExecutable(t, path, []byte("#!/bin/sh\nexit 0\n"))
	if out, err := exec.Command(path).CombinedOutput(); err != nil {
		t.Fatalf("exec of a rewritten file that existed as 0o644: %v\n%s", err, out)
	}
}

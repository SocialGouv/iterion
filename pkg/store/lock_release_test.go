package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Deleting a run whose lock is still held (a runner releasing it, a
// reconcile probe) deletes its content and leaves the lock file to the
// tombstone: on Windows a held lock file cannot be removed.
func TestDeleteRunContent_LeavesAHeldLockToTheTombstone(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	held, err := s.LockRun(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Unlock() }()
	if err := os.WriteFile(filepath.Join(s.runDir("r1"), "run.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteRunContent("r1"); err != nil {
		t.Fatalf("deleteRunContent with the run lock held = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(s.runDir("r1"), "run.json")); !os.IsNotExist(err) {
		t.Fatalf("run.json survived the delete (stat err=%v)", err)
	}
}

// A lock released while another handle has its file open is released all
// the same. On Windows the pid-in-a-file lock could not delete its file
// under an open handle, and the lock outlived its Unlock, naming this live
// process, until restart.
func TestLockFile_ReleasedWhileItsFileIsOpenElsewhere(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	held, err := lockFile(path, "probe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockFile(path, "probe"); err == nil {
		t.Fatal("a second lock on a held path succeeded")
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := held.Unlock(); err != nil {
		t.Fatalf("Unlock with the file open elsewhere = %v", err)
	}
	again, err := lockFile(path, "probe")
	if err != nil {
		t.Fatalf("lock after Unlock = %v, want it free", err)
	}
	_ = again.Unlock()
}

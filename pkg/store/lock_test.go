package store

import (
	"context"
	"testing"
)

func TestLockAndUnlock(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun(context.Background(), "run-lock", "wf", nil)

	lock, err := s.LockRun(context.Background(), "run-lock")
	if err != nil {
		t.Fatalf("LockRun: %v", err)
	}

	if err := lock.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
}

func TestLockBlocksConcurrent(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun(context.Background(), "run-block", "wf", nil)

	// Acquire the first lock.
	lock1, err := s.LockRun(context.Background(), "run-block")
	if err != nil {
		t.Fatalf("LockRun 1: %v", err)
	}
	defer lock1.Unlock()

	// Second lock attempt should fail (non-blocking on Unix, exclusive on Windows).
	_, err = s.LockRun(context.Background(), "run-block")
	if err == nil {
		t.Fatal("expected error for concurrent lock, got nil")
	}
}

func TestLockReleasedAfterUnlock(t *testing.T) {
	s := tmpStore(t)
	s.CreateRun(context.Background(), "run-relock", "wf", nil)

	// Acquire and release.
	lock1, err := s.LockRun(context.Background(), "run-relock")
	if err != nil {
		t.Fatalf("LockRun 1: %v", err)
	}
	if err := lock1.Unlock(); err != nil {
		t.Fatalf("Unlock 1: %v", err)
	}

	// Should be able to re-acquire.
	lock2, err := s.LockRun(context.Background(), "run-relock")
	if err != nil {
		t.Fatalf("LockRun 2 after unlock: %v", err)
	}
	defer lock2.Unlock()
}

func TestLockRejectsPathTraversal(t *testing.T) {
	s := tmpStore(t)
	_, err := s.LockRun(context.Background(), "../../../etc")
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
}

package store

import (
	"context"
	"testing"
)

// LockRun mkdirs the run directory to place its .lock, so an id that is locked
// and never created leaves a directory holding only that lock. Listed as a run,
// it is a permanent phantom: it never loads, and a consumer that inspects the
// first id it is handed waits forever on it while the real run sits behind it.
func TestListRuns_SkipsALockedButNeverCreatedRun(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	lock, err := s.LockRun(ctx, "01-locked-never-created")
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer func() { _ = lock.Unlock() }()

	if _, err := s.CreateRun(ctx, "02-real", "wf", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	ids, err := s.ListRuns(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ids) != 1 || ids[0] != "02-real" {
		t.Fatalf("only the created run is a run; got %v", ids)
	}
	// The guarantee that matters downstream: every id listed loads.
	for _, id := range ids {
		if _, err := s.LoadRun(ctx, id); err != nil {
			t.Fatalf("listed id %s must load, got %v", id, err)
		}
	}
}

// A run that IS created keeps its lock without becoming unlistable — the skip
// keys on run.json, not on the absence of a lock.
func TestListRuns_ListsACreatedRunThatHoldsItsLock(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "01-real", "wf", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	lock, err := s.LockRun(ctx, "01-real")
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer func() { _ = lock.Unlock() }()

	ids, err := s.ListRuns(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ids) != 1 || ids[0] != "01-real" {
		t.Fatalf("a locked, created run stays listed; got %v", ids)
	}
}

package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// RunAwaitAnswersWaitConformance is shared by the filesystem and Mongo suites.
func RunAwaitAnswersWaitConformance(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	const id = "await-wait-conformance"
	if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatal(err)
	}
	// Mongo creates queued runs; the engine must own the execution first.
	if err := s.UpdateRunStatus(ctx, id, store.RunStatusRunning, ""); err != nil {
		t.Fatal(err)
	}
	wait := store.AwaitAnswersWait{NodeID: "sync", Until: time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)}
	stale, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for _, token := range []string{"branch-a", "branch-b"} {
		go func() { results <- s.SetAwaitAnswersWait(ctx, id, token, &wait) }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	read := func(t *testing.T) *store.Run {
		t.Helper()
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if got := read(t); len(got.AwaitAnswersWaits) != 2 {
		t.Fatalf("concurrent waits clobbered: %+v", got.AwaitAnswersWaits)
	}
	if err := s.SaveRun(ctx, stale); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("stale save could erase wait proof: %v", err)
	}
	if err := s.SetAwaitAnswersWait(ctx, id, "branch-a", nil); err != nil {
		t.Fatal(err)
	}
	if got := read(t); len(got.AwaitAnswersWaits) != 1 || got.AwaitAnswersWaits["branch-b"].NodeID != "sync" {
		t.Fatalf("clearing one invocation removed its sibling: %+v", got.AwaitAnswersWaits)
	}
	if !store.HasBlockingHumanWait(ctx, s, id, time.Now()) {
		t.Fatal("live wait not observed")
	}
	if store.HasBlockingHumanWait(ctx, s, id, wait.Until) {
		t.Fatal("expired proof disarmed the watchdog")
	}
	for _, token := range []string{"", "bad.key", "$bad", "bad\x00key"} {
		if err := s.SetAwaitAnswersWait(ctx, id, token, &wait); err == nil {
			t.Fatalf("accepted unsafe token %q", token)
		}
	}
	if err := s.SetAwaitAnswersWait(ctx, "missing", "token", &wait); err == nil {
		t.Fatal("marked a missing run")
	}

	transitions := map[string]func(*testing.T) error{
		"pause":  func(*testing.T) error { return s.PauseRun(ctx, id, &store.Checkpoint{NodeID: "gate"}) },
		"cancel": func(*testing.T) error { return s.UpdateRunStatus(ctx, id, store.RunStatusCancelled, "cancelled") },
		"failure": func(*testing.T) error {
			return s.FailRunResumable(ctx, id, &store.Checkpoint{NodeID: "sync"}, "failed", store.FailureExecutionFailed)
		},
		"save-queued": func(t *testing.T) error { r := read(t); r.Status = store.RunStatusQueued; return s.SaveRun(ctx, r) },
	}
	for name, transition := range transitions {
		t.Run(name, func(t *testing.T) {
			if err := s.UpdateRunStatus(ctx, id, store.RunStatusRunning, ""); err != nil {
				t.Fatal(err)
			}
			if err := s.SetAwaitAnswersWait(ctx, id, "active", &wait); err != nil {
				t.Fatal(err)
			}
			if err := transition(t); err != nil {
				t.Fatal(err)
			}
			if got := read(t); len(got.AwaitAnswersWaits) != 0 {
				t.Fatalf("wait outlived its execution: %+v", got.AwaitAnswersWaits)
			}
			if err := s.SetAwaitAnswersWait(ctx, id, "late-writer", &wait); err == nil {
				t.Fatal("late writer parked a non-running run")
			}
			if err := s.SetAwaitAnswersWait(ctx, id, "active", nil); err != nil {
				t.Fatalf("cleanup after terminal: %v", err)
			}
			if err := s.UpdateRunStatus(ctx, id, store.RunStatusRunning, ""); err != nil {
				t.Fatal(err)
			}
			if store.HasBlockingHumanWait(ctx, s, id, time.Now()) {
				t.Fatal("old marker exempted a new execution")
			}
		})
	}
	if err := s.DeleteRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, w := range []*store.AwaitAnswersWait{&wait, nil} {
		if err := s.SetAwaitAnswersWait(ctx, id, "late", w); err == nil {
			t.Fatal("wait write resurrected a deleted run")
		}
	}
}

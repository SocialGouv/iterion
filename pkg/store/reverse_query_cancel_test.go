package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// The filesystem store answers its indexed questions by scanning every run,
// and LoadRun ignores ctx — so the scan itself is the one place a cancelled
// caller is heard. A sweep drained at shutdown, or a project switch, must
// get its cancellation back from the first run, not after the whole tree
// has been read (#2636 moved the sweep's per-run loop into this scan).
func TestReverseQueryScanHonoursCancellation(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	for i := range 20 {
		id := fmt.Sprintf("run-%02d", i)
		if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		if _, err := s.AddWatchedIssues(ctx, id, []string{"native:card"}); err != nil {
			t.Fatalf("AddWatchedIssues %s: %v", id, err)
		}
	}

	live, err := s.ListDeliverableWatchers(ctx)
	if err != nil || len(live) != 20 {
		t.Fatalf("ListDeliverableWatchers (live ctx) = %d ids, %v; want 20 — the control must see the scan work", len(live), err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for name, query := range map[string]func(context.Context) ([]string, error){
		"ListDeliverableWatchers": s.ListDeliverableWatchers,
		"ListRunsBySourceIssue":   func(ctx context.Context) ([]string, error) { return s.ListRunsBySourceIssue(ctx, "native:card") },
		"ListChildRuns":           func(ctx context.Context) ([]string, error) { return s.ListChildRuns(ctx, "run-00") },
	} {
		got, err := query(cancelled)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s on a cancelled ctx returned %d ids, err %v — want context.Canceled before the tree is read", name, len(got), err)
		}
	}
}

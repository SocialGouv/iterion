package usernotify

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestSweepRecoversMissedPause simulates the lossy-bus gap: a run paused
// while no replica was subscribed, so no live event fired. The sweep must
// deliver the notification exactly once across repeated passes.
func TestSweepRecoversMissedPause(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sink := &captureSink{name: "capture"}
	d := NewDispatcher(st, nil, NewMemSentStore(), "https://iterion.example", nil, sink)

	// Paused run persisted, but its bus event was never handled.
	_ = pausedRun(t, st, "run-missed")

	list := func(ctx context.Context, _ time.Time, _ int) ([]RunRef, error) {
		return []RunRef{{ID: "run-missed", TenantID: "team-1", Status: string(store.RunStatusPausedWaitingHuman)}}, nil
	}
	sw := NewSweeper(d, list, nil)

	sw.SweepOnce(context.Background())
	if sink.calls != 1 {
		t.Fatalf("calls after first sweep = %d, want 1", sink.calls)
	}
	n := sink.last(t)
	if n.Kind != KindHumanInputRequested || n.RunID != "run-missed" {
		t.Fatalf("unexpected notification: %+v", n)
	}

	// Subsequent passes are idempotent.
	sw.SweepOnce(context.Background())
	sw.SweepOnce(context.Background())
	if sink.calls != 1 {
		t.Fatalf("calls after replays = %d, want 1", sink.calls)
	}
}

// TestSweepAfterLiveDelivery asserts the bus path and the sweep share one
// episode claim: a live-handled event is not re-sent by the sweep.
func TestSweepAfterLiveDelivery(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sink := &captureSink{name: "capture"}
	d := NewDispatcher(st, nil, NewMemSentStore(), "", nil, sink)

	ev := pausedRun(t, st, "run-live")
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	list := func(ctx context.Context, _ time.Time, _ int) ([]RunRef, error) {
		return []RunRef{{ID: "run-live", TenantID: "team-1", Status: string(store.RunStatusPausedWaitingHuman)}}, nil
	}
	NewSweeper(d, list, nil).SweepOnce(context.Background())
	if sink.calls != 1 {
		t.Fatalf("calls = %d, want 1 (sweep must dedup against live path)", sink.calls)
	}
}

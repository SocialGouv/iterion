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

	list := func(ctx context.Context, _, _ time.Time, _ int) ([]RunRef, error) {
		return []RunRef{{ID: "run-missed", Status: string(store.RunStatusPausedWaitingHuman)}}, nil
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

	// The ref carries the pending interaction + updated_at (as the Mongo
	// listing does), so the sweep's cheap pre-check derives the SAME
	// episode key as the live event and skips without a run load.
	r, err := st.LoadRun(context.Background(), "run-live")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	list := func(ctx context.Context, _, _ time.Time, _ int) ([]RunRef, error) {
		return []RunRef{{ID: "run-live", Status: string(store.RunStatusPausedWaitingHuman), InteractionID: "run-live_ask", UpdatedAt: r.UpdatedAt}}, nil
	}
	NewSweeper(d, list, nil).SweepOnce(context.Background())
	if sink.calls != 1 {
		t.Fatalf("calls = %d, want 1 (sweep must dedup against live path)", sink.calls)
	}
}

// TestClaimGraceRecoversAbandonedEpisode pins the two-phase claim: a
// pending claim whose owner died mid-delivery shields the episode only for
// ClaimGrace, then is taken over and retried; a DELIVERED episode is
// settled forever.
func TestClaimGraceRecoversAbandonedEpisode(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sink := &captureSink{name: "capture"}
	sent := NewMemSentStore()
	d := NewDispatcher(st, nil, sent, "", nil, sink)
	ctx := context.Background()

	ev := pausedRun(t, st, "run-crash")

	// Another pod claimed the episode, then died before delivering.
	if won, _ := sent.TryMark(ctx, ev.ID); !won {
		t.Fatal("initial claim should win")
	}
	// Within the grace the claim shields the episode.
	if err := d.Handle(ctx, ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if sink.calls != 0 {
		t.Fatalf("calls = %d, want 0 (fresh pending claim must shield)", sink.calls)
	}
	// Past the grace the abandoned claim is taken over and delivery runs.
	sent.now = func() time.Time { return time.Now().Add(ClaimGrace + time.Minute) }
	if err := d.Handle(ctx, ev); err != nil {
		t.Fatalf("Handle after grace: %v", err)
	}
	if sink.calls != 1 {
		t.Fatalf("calls = %d, want 1 (stale pending claim must be retried)", sink.calls)
	}
	// Delivery confirmed the claim: even past any grace it stays settled.
	sent.now = func() time.Time { return time.Now().Add(10 * ClaimGrace) }
	if err := d.Handle(ctx, ev); err != nil {
		t.Fatalf("Handle after delivery: %v", err)
	}
	if sink.calls != 1 {
		t.Fatalf("calls = %d, want 1 (delivered episode must never re-send)", sink.calls)
	}
}

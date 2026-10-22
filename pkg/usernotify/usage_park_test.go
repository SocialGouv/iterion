package usernotify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// parkedOnUsageWindow persists the run shape a usage-window park leaves: a
// failed_resumable run whose retry row carries the provider's wall. Every
// later step of the tests below rewrites that row the way the store's own
// retry writers do, and stamps updated_at, because the episode key the
// dispatcher dedups on is derived from the persisted document.
func parkedOnUsageWindow(t *testing.T, st *store.FilesystemRunStore, id string, wall, updatedAt time.Time) *store.Run {
	t.Helper()
	ctx := context.Background()
	r, err := st.CreateRun(ctx, id, "weekly-digest", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r.TenantID = "team-1"
	r.OwnerID = "owner-1"
	r.Name = "Review release"
	r.Status = store.RunStatusFailedResumable
	r.Error = "node \"campaign\": rate_limited: You've hit your weekly limit"
	r.Checkpoint = &store.Checkpoint{NodeID: "campaign"}
	r.ContinuationState = store.ContinuationRetryArmed
	after := wall
	r.RetryState = &store.RunRetryState{
		RetryAfter: &after,
		Wall:       &wall,
		Reason:     store.RetryReasonUsageWindow,
		Code:       "USAGE_LIMIT_BLOCKED",
		Attempts:   1,
	}
	r.UpdatedAt = updatedAt
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	return r
}

// saveRow rewrites the retry row and the document's updated_at, as one of the
// store's retry writers does.
func saveRow(t *testing.T, st *store.FilesystemRunStore, r *store.Run, updatedAt time.Time) {
	t.Helper()
	r.UpdatedAt = updatedAt
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
}

// TestUsagePark_OneNotificationPerProviderWall is the guarantee #2451 asks
// for: the unit of a usage park is the provider WALL, so a run that parks
// several times on one window reaches its owner once — and says it is waiting,
// not that it failed.
//
// The sequence is the production one: the park that opens the wall, the
// sweeper's pull-forward probe, and the free re-arm that follows the probe's
// own refusal. Each rewrites the row and bumps updated_at, which is what makes
// every one of them a fresh episode.
func TestUsagePark_OneNotificationPerProviderWall(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sink := &captureSink{name: "capture"}
	d := NewDispatcher(st, NewMemPrefsStore(), NewMemSentStore(), "https://iterion.example/", nil, sink)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)
	wall := base.Add(4 * 24 * time.Hour)
	run := parkedOnUsageWindow(t, st, "run-wall-1", wall, base)

	handle := func(step string) {
		t.Helper()
		if err := d.Handle(ctx, trigger.BuildRunOutcome(ctx, st, run.ID, nil)); err != nil {
			t.Fatalf("Handle after %s: %v", step, err)
		}
	}

	handle("the park that opened the wall")
	if len(sink.seen) != 1 {
		t.Fatalf("the park that opened the wall delivered %d notifications, want 1", len(sink.seen))
	}
	n := sink.seen[0]
	if n.Kind != KindUsageWindowParked {
		t.Errorf("kind = %q, want %q: a run waiting on a provider window has not failed", n.Kind, KindUsageWindowParked)
	}
	if got := n.Data["usage_park_wall"]; got != wall.Format(time.RFC3339) {
		// What this pins is the dispatcher's own output: the wall travels on
		// the notification rather than being re-derived from its text. Data
		// is the structured half of the Sink contract (node_id and
		// interaction_id ride the same way) and today's one sink, webpush,
		// serializes only the rendered fields.
		t.Errorf("the notification carries usage_park_wall=%q, want %q", got, wall.Format(time.RFC3339))
	}
	if strings.Contains(n.Title, "failed") || strings.Contains(n.Body, "failed") {
		t.Errorf("a usage park reads as a failure:\n  title: %q\n  body:  %q", n.Title, n.Body)
	}
	if !strings.Contains(n.Body, wall.Format("15:04 UTC on 2006-01-02")) {
		t.Errorf("body %q does not name the wall %s it resumes at", n.Body, wall.Format(time.RFC3339))
	}

	// The sweeper's probe pulls the row forward and holds the wall aside.
	probeAt := base.Add(time.Minute)
	run.RetryState.RetryAfter = &probeAt
	held := wall
	run.RetryState.ProbeFrom = &held
	run.RetryState.LastProbeAt = &probeAt
	run.RetryState.ProbeCount = 1
	saveRow(t, st, run, probeAt)

	// The probe's resume meets the same window: the re-arm puts the wall
	// back, free of charge. Nothing new happened to this run's owner.
	rearmed := wall
	run.RetryState.RetryAfter = &rearmed
	run.RetryState.ProbeFrom = nil
	saveRow(t, st, run, base.Add(2*time.Minute))

	handle("the free re-arm that followed the probe")
	if len(sink.seen) != 1 {
		t.Fatalf("a probe's re-park delivered notification %d; want 1 per wall — the owner is told their run failed once per probe of the budget, and nothing failed",
			len(sink.seen))
	}

	// A NEW wall is news: the provider named a different instant, and the
	// park that opened it spends an attempt.
	nextWall := base.Add(7 * 24 * time.Hour)
	run.RetryState.RetryAfter = &nextWall
	run.RetryState.Wall = &nextWall
	run.RetryState.Attempts = 2
	run.RetryState.ProbeCount = 0
	run.RetryState.LastProbeAt = nil
	saveRow(t, st, run, base.Add(3*time.Minute))

	handle("the park that opened a new wall")
	if len(sink.seen) != 2 {
		t.Fatalf("a new provider wall delivered %d notifications in total, want 2: suppressing a park that opens a wall nobody has heard of leaves the owner with a silent run", len(sink.seen))
	}
	if got := sink.seen[1].Body; !strings.Contains(got, nextWall.Format("15:04 UTC on 2006-01-02")) {
		t.Errorf("the second notification's body %q does not name the new wall %s", got, nextWall.Format(time.RFC3339))
	}

	// And the shape that must NOT be read as a park: the budget is spent, so
	// nothing is armed any more. That run really has stopped, and its owner
	// is owed the failure.
	run.RetryState.RetryAfter = nil
	run.RetryState.LastError = "usage-window retries exhausted (max 5)"
	run.ContinuationState = store.ContinuationFinal
	saveRow(t, st, run, base.Add(4*time.Minute))
	handle("the exhausted budget")
	if len(sink.seen) != 3 {
		t.Fatalf("an exhausted retry delivered %d notifications in total, want 3: a run nothing will resume is a failure, and suppressing it is how it goes quiet", len(sink.seen))
	}
	if got := sink.seen[2].Kind; got != KindRunFailed {
		t.Errorf("kind = %q for a run whose retry budget is spent, want %q", got, KindRunFailed)
	}
}

// TestUsagePark_TheSweepPreCheckDerivesTheSameEpisode pins the pair that makes
// the reconciliation sweep idempotent against the bus path: the key the sweep
// computes from its listing alone must be the one BuildRunOutcome mints from
// the run document, or every usage-parked run costs a run load on every pass
// and the claim it checks is a key nobody ever writes.
func TestUsagePark_TheSweepPreCheckDerivesTheSameEpisode(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	wall := base.Add(36 * time.Hour)
	run := parkedOnUsageWindow(t, st, "run-wall-2", wall, base)

	ev := trigger.BuildRunOutcome(ctx, st, run.ID, nil)
	ref := RunRef{
		ID:        run.ID,
		Status:    string(store.RunStatusFailedResumable),
		UpdatedAt: base,
		UsageWall: wall,
	}
	if got, want := ref.Episode(), ev.ID; got != want {
		t.Fatalf("the sweep derives episode %q, the event path mints %q", got, want)
	}
	// And the key does NOT move when the row's bookkeeping does: a probe
	// pull, a lease and a free re-arm each rewrite updated_at.
	moved := RunRef{ID: run.ID, Status: ref.Status, UpdatedAt: base.Add(time.Hour), UsageWall: wall}
	if got := moved.Episode(); got != ref.Episode() {
		t.Fatalf("the episode moved with updated_at: %q then %q", ref.Episode(), got)
	}
	// A run that is NOT parked on a wall keeps the per-transition episode.
	plain := RunRef{ID: run.ID, Status: string(store.RunStatusFailed), UpdatedAt: base}
	if got, want := plain.Episode(), trigger.RunOutcomeEventID(run.ID, plain.Status, "", base); got != want {
		t.Fatalf("a terminal failure derives %q, want the per-transition %q", got, want)
	}
}

// TestUsagePark_TheBodyNamesTheInstantTheRunComesBack: the wall identifies the
// episode, and the body has to name the WAKE — they are not the same instant.
//
// The arming takes the EARLIER of the failed credential's own reset and a
// credential the launch's resolution passed over (store.Run
// SkippedCredReopensAt, the measured shape: a team key refused on its
// five-hour window reopens the same afternoon while the platform forfait it
// fell through to is walled until Monday), then applies the jitter, the
// five-minute floor and the max_wait ceiling. So retry_after can be days
// before the wall — and a body rendered from the wall told the owner to expect
// their run on the wrong day, which is the one thing this notification exists
// to say.
func TestUsagePark_TheBodyNamesTheInstantTheRunComesBack(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sink := &captureSink{name: "capture"}
	d := NewDispatcher(st, NewMemPrefsStore(), NewMemSentStore(), "https://iterion.example/", nil, sink)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)
	wall := base.Add(72 * time.Hour)
	run := parkedOnUsageWindow(t, st, "run-wake-1", wall, base)
	// The skipped credential reopens this afternoon: that is when this row
	// wakes, while the wall it is parked on is Monday's.
	wake := base.Add(3 * time.Hour)
	run.RetryState.RetryAfter = &wake
	saveRow(t, st, run, base)

	ev := trigger.BuildRunOutcome(ctx, st, run.ID, nil)
	if got, want := ev.ID, trigger.RunUsageParkEventID(run.ID, wall); got != want {
		t.Fatalf("episode = %q, want the WALL-keyed %q: the identity is the window, not the wake", got, want)
	}
	if err := d.Handle(ctx, ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(sink.seen) != 1 {
		t.Fatalf("delivered %d notifications, want 1", len(sink.seen))
	}
	n := sink.seen[0]
	if !strings.Contains(n.Body, wake.Format("15:04 UTC on 2006-01-02")) {
		t.Errorf("body %q does not name %s, the instant the row is ARMED for — the wall is %s, %s later, and a reader told that waits three days for a run that comes back this afternoon",
			n.Body, wake.Format(time.RFC3339), wall.Format(time.RFC3339), wall.Sub(wake))
	}
	if strings.Contains(n.Body, wall.Format("15:04 UTC on 2006-01-02")) {
		t.Errorf("body %q names the WALL (%s) rather than the wake (%s)", n.Body, wall.Format(time.RFC3339), wake.Format(time.RFC3339))
	}
	if got := n.Data["usage_park_wall"]; got != wall.Format(time.RFC3339) {
		t.Errorf("usage_park_wall = %q, want the episode's own wall %q", got, wall.Format(time.RFC3339))
	}
	if got := n.Data["usage_park_resumes_at"]; got != wake.Format(time.RFC3339) {
		t.Errorf("usage_park_resumes_at = %q, want %q", got, wake.Format(time.RFC3339))
	}
}

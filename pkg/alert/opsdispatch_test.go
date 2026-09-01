package alert_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/alert"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/usernotify"
)

// captureSink records delivered alerts.
type captureSink struct {
	mu   sync.Mutex
	seen []alert.Alert
}

func (c *captureSink) Notify(_ context.Context, a alert.Alert) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, a)
}

func (c *captureSink) alerts() []alert.Alert {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]alert.Alert(nil), c.seen...)
}

func opsWorld(t *testing.T) (*alert.OpsDispatcher, *store.FilesystemRunStore, *captureSink) {
	t.Helper()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	d := &alert.OpsDispatcher{
		Runs: rs,
		// usernotify's claim store IS the EpisodeClaims contract — using it
		// here pins the structural compatibility the wiring relies on.
		Claims:  usernotify.NewMemSentStore(),
		Sinks:   []alert.Sink{sink},
		BaseURL: "https://iterion.example.com",
	}
	return d, rs, sink
}

func seedOpsRun(t *testing.T, rs *store.FilesystemRunStore, status store.RunStatus, mut func(*store.Run)) *store.Run {
	t.Helper()
	id, err := store.GenerateRunID()
	if err != nil {
		t.Fatal(err)
	}
	run, err := rs.CreateRun(context.Background(), id, "feed-watch", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = status
	run.Name = "cyber digest"
	if mut != nil {
		mut(run)
	}
	if err := rs.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

// TestOpsDispatcher_ParkedRunAlertsOnceWithETA is the 2026-08-31 scenario:
// a digest parks failed_resumable on a usage window with a retry armed. The
// operator webhook must say so ONCE — with the reason and the reset ETA —
// however many times the episode is offered (bus + sweep + replicas).
func TestOpsDispatcher_ParkedRunAlertsOnceWithETA(t *testing.T) {
	d, rs, sink := opsWorld(t)
	reset := time.Now().Add(3 * time.Hour).UTC()
	run := seedOpsRun(t, rs, store.RunStatusFailedResumable, func(r *store.Run) {
		r.Error = "usage cap: weekly window at 70%"
		r.RetryState = &store.RunRetryState{RetryAfter: &reset, Reason: "usage_window", Code: "USAGE_LIMIT_BLOCKED", Attempts: 1}
	})

	ev := trigger.BuildRunOutcome(context.Background(), rs, run.ID, nil)
	if ev.Kind != trigger.KindRunFailed {
		t.Fatalf("outcome kind = %s, want run.failed", ev.Kind)
	}
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("handle: %v", err)
	}
	// Same episode again (sweep replay / second replica): deduped.
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("re-handle: %v", err)
	}

	got := sink.alerts()
	if len(got) != 1 {
		t.Fatalf("delivered %d alerts, want exactly 1", len(got))
	}
	a := got[0]
	if a.Kind != alert.KindRunParked {
		t.Fatalf("kind = %s, want run_parked", a.Kind)
	}
	for _, want := range []string{"usage_window", reset.Format(time.RFC3339), "usage cap"} {
		if !strings.Contains(a.Reason+a.WebhookText(), want) {
			t.Fatalf("alert text misses %q: %s", want, a.WebhookText())
		}
	}
	if !strings.Contains(a.Link, run.ID) {
		t.Fatalf("no deep link to the run: %q", a.Link)
	}
}

// TestOpsDispatcher_SweepRepaysTheDroppedEvent: the lossy bus dropped the
// outcome; the sweep window re-offers it and the alert still goes out.
func TestOpsDispatcher_SweepRepaysTheDroppedEvent(t *testing.T) {
	d, rs, sink := opsWorld(t)
	run := seedOpsRun(t, rs, store.RunStatusFailedResumable, func(r *store.Run) {
		r.Error = "provider window closed"
	})

	list := func(_ context.Context, _, _ time.Time, _ int) ([]usernotify.RunRef, error) {
		return []usernotify.RunRef{{ID: run.ID, Status: string(run.Status), UpdatedAt: run.UpdatedAt}}, nil
	}
	d.SweepOnce(context.Background(), list)
	if len(sink.alerts()) != 1 {
		t.Fatalf("sweep delivered %d alerts, want 1", len(sink.alerts()))
	}
	// A second pass over the same window is a cheap no-op.
	d.SweepOnce(context.Background(), list)
	if len(sink.alerts()) != 1 {
		t.Fatal("sweep re-delivered a claimed episode")
	}
}

// TestOpsDispatcher_ClassificationBoundaries: hard failures alert as
// run_failed; anything non-failed alerts nothing.
func TestOpsDispatcher_ClassificationBoundaries(t *testing.T) {
	d, rs, sink := opsWorld(t)

	hard := seedOpsRun(t, rs, store.RunStatusFailed, func(r *store.Run) { r.Error = "FailNode reached" })
	_ = d.Handle(context.Background(), trigger.BuildRunOutcome(context.Background(), rs, hard.ID, nil))
	if got := sink.alerts(); len(got) != 1 || got[0].Kind != alert.KindRunFailed {
		t.Fatalf("hard failure: %+v", got)
	}

	fin := seedOpsRun(t, rs, store.RunStatusFinished, nil)
	_ = d.Handle(context.Background(), trigger.BuildRunOutcome(context.Background(), rs, fin.ID, nil))
	if len(sink.alerts()) != 1 {
		t.Fatal("a finished run produced an operator alert")
	}
}

// failingSink is an ErrorReportingSink with a switchable fault — the
// transient-channel-outage shape (Mattermost rolling restart, ingress 502).
type failingSink struct {
	captureSink
	err error
}

func (f *failingSink) NotifyErr(ctx context.Context, a alert.Alert) error {
	if f.err != nil {
		return f.err
	}
	f.Notify(ctx, a)
	return nil
}

// TestOpsDispatcher_RetryBookkeepingDoesNotRespam pins the stable episode
// key: the retry cycle rewrites a parked run every sweep-minute
// (ScheduleRunRetry/ClaimRunRetry bump updated_at while the status never
// leaves failed_resumable) — the operator must get ONE message per real
// retry cycle, not one per bookkeeping write.
func TestOpsDispatcher_RetryBookkeepingDoesNotRespam(t *testing.T) {
	d, rs, sink := opsWorld(t)
	reset := time.Now().Add(3 * time.Hour).UTC()
	run := seedOpsRun(t, rs, store.RunStatusFailedResumable, func(r *store.Run) {
		r.Error = "usage cap: weekly window"
		r.RetryState = &store.RunRetryState{RetryAfter: &reset, Reason: "usage_window", Attempts: 1}
	})
	_ = d.Handle(context.Background(), trigger.BuildRunOutcome(context.Background(), rs, run.ID, nil))

	// A claim/re-arm write: updated_at moves, status and attempts do not.
	run2, _ := rs.LoadRun(context.Background(), run.ID)
	run2.UpdatedAt = run2.UpdatedAt.Add(90 * time.Second)
	_ = rs.SaveRun(context.Background(), run2)
	_ = d.Handle(context.Background(), trigger.BuildRunOutcome(context.Background(), rs, run.ID, nil))
	if got := len(sink.alerts()); got != 1 {
		t.Fatalf("%d alerts after a bookkeeping bump, want 1 — this is the Mattermost-killing spam", got)
	}

	// A REAL new retry cycle (attempts advanced) alerts once more.
	run3, _ := rs.LoadRun(context.Background(), run.ID)
	run3.RetryState.Attempts = 2
	run3.UpdatedAt = run3.UpdatedAt.Add(4 * time.Hour)
	_ = rs.SaveRun(context.Background(), run3)
	_ = d.Handle(context.Background(), trigger.BuildRunOutcome(context.Background(), rs, run.ID, nil))
	if got := len(sink.alerts()); got != 2 {
		t.Fatalf("%d alerts after a new retry cycle, want 2", got)
	}
}

// TestOpsDispatcher_TransientChannelOutageRetries pins the Unmark contract:
// a 15-second receiver outage must not consume the one alert this component
// exists to deliver — the claim is released and the sweep redelivers.
func TestOpsDispatcher_TransientChannelOutageRetries(t *testing.T) {
	d, rs, _ := opsWorld(t)
	sink := &failingSink{err: context.DeadlineExceeded}
	d.Sinks = []alert.Sink{sink}
	run := seedOpsRun(t, rs, store.RunStatusFailed, func(r *store.Run) { r.Error = "boom" })
	ev := trigger.BuildRunOutcome(context.Background(), rs, run.ID, nil)

	_ = d.Handle(context.Background(), ev) // channel down — claim must release
	if got := len(sink.alerts()); got != 0 {
		t.Fatalf("delivered %d during the outage?", got)
	}
	sink.err = nil
	_ = d.Handle(context.Background(), ev) // the sweep's replay
	if got := len(sink.alerts()); got != 1 {
		t.Fatalf("%d alerts after the channel recovered, want 1 — a 15s outage silenced the episode forever", got)
	}
}

// TestOpsDispatcher_SweepPaginatesPastAFullPage pins the keyset cursor: a
// burst of newer (already-claimed) episodes larger than one page must not
// starve the oldest unclaimed run forever.
func TestOpsDispatcher_SweepPaginatesPastAFullPage(t *testing.T) {
	d, rs, sink := opsWorld(t)
	old := seedOpsRun(t, rs, store.RunStatusFailedResumable, func(r *store.Run) { r.Error = "oldest, unclaimed" })

	// Page 1: a full page of newer refs whose episodes are already settled
	// (pre-marked), so they cost only the IsMarked pre-check.
	now := time.Now().UTC()
	var page1 []usernotify.RunRef
	for i := 0; i < 500; i++ {
		ref := usernotify.RunRef{ID: fmt.Sprintf("newer-%03d", i), Status: string(store.RunStatusFailedResumable), UpdatedAt: now.Add(-time.Duration(i) * time.Second)}
		key := "ops|" + trigger.RunOutcomeEventID(ref.ID, ref.Status, "", ref.UpdatedAt)
		if won, _ := d.Claims.TryMark(context.Background(), key); won {
			_ = d.Claims.MarkDelivered(context.Background(), key)
		}
		page1 = append(page1, ref)
	}
	pages := 0
	list := func(_ context.Context, _, before time.Time, _ int) ([]usernotify.RunRef, error) {
		pages++
		if before.IsZero() {
			return page1, nil
		}
		return []usernotify.RunRef{{ID: old.ID, Status: string(old.Status), UpdatedAt: old.UpdatedAt}}, nil
	}
	d.SweepOnce(context.Background(), list)
	if pages < 2 {
		t.Fatalf("sweep stopped after %d page(s) — the oldest episode is starved forever", pages)
	}
	if got := len(sink.alerts()); got != 1 {
		t.Fatalf("%d alerts, want 1 (the starved oldest run)", got)
	}
}

// TestOpsDispatcher_LoserNeverCertifiesAPendingClaim pins the round-2 race:
// replica A wins the episode claim and is mid-fan-out when replica B's offer
// loses — B must NOT stamp the transition marker off A's PENDING claim,
// because A may fail every sink and release; a stamped marker would make
// every later sweep skip the released episode before Handle runs, silencing
// a parked-no-retry alert forever.
func TestOpsDispatcher_LoserNeverCertifiesAPendingClaim(t *testing.T) {
	d, rs, _ := opsWorld(t)
	sink := &failingSink{err: context.DeadlineExceeded}
	d.Sinks = []alert.Sink{sink}
	run := seedOpsRun(t, rs, store.RunStatusFailedResumable, func(r *store.Run) {
		r.Error = "no automatic retry armed"
	})
	ev := trigger.BuildRunOutcome(context.Background(), rs, run.ID, nil)

	// Replica A: wins, all sinks fail, claim released (Unmark).
	// Replica B (interleaved): loses while A's claim is still pending.
	// Simulate B's offer between A's TryMark and A's Unmark by pre-claiming.
	epWon, _ := d.Claims.TryMark(context.Background(), "ops|run:"+run.ID+":parked:0")
	if !epWon {
		t.Fatal("setup: could not pre-claim the episode")
	}
	_ = d.Handle(context.Background(), ev)                                   // B: loses, must not stamp evKey
	_ = d.Claims.Unmark(context.Background(), "ops|run:"+run.ID+":parked:0") // A fails → releases

	// The sweep replays with a healthy channel: the alert MUST go out.
	sink.err = nil
	list := func(_ context.Context, _, _ time.Time, _ int) ([]usernotify.RunRef, error) {
		return []usernotify.RunRef{{ID: run.ID, Status: string(run.Status), UpdatedAt: run.UpdatedAt}}, nil
	}
	d.SweepOnce(context.Background(), list)
	if got := len(sink.alerts()); got != 1 {
		t.Fatalf("%d alerts after release + healthy sweep, want 1 — the loser's stamp silenced the episode forever", got)
	}
}

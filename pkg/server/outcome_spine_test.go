package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/usernotify"
)

// spineFixture is a server wired with the four record stores the spine
// stamps, plus a scripted terminal-run listing standing in for the cloud
// store's window scan.
type spineFixture struct {
	srv    *Server
	sched  *cloudsched.MemoryStore
	listed []usernotify.RunRef
	// calls counts the listing pages the sweep asked for.
	calls int
}

func newSpineFixture(t *testing.T) *spineFixture {
	t.Helper()
	workDir := t.TempDir()
	srv := New(Config{
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelError, nil))
	f := &spineFixture{srv: srv, sched: cloudsched.NewMemoryStore()}
	srv.cfg.ScheduledBots = f.sched
	srv.cfg.NotifiableRuns = func(_ context.Context, _, _ time.Time, _ int) ([]usernotify.RunRef, error) {
		f.calls++
		return f.listed, nil
	}
	return f
}

// seedRun writes a run in `status` whose document was last touched at
// `at`, back-referenced to `scheduleID`, and returns the listing row the
// cloud scan would produce for it. Any status — the non-terminal rows are
// what the gates are witnessed against.
func (f *spineFixture) seedRun(t *testing.T, runID, tenantID, scheduleID string, status store.RunStatus, errMsg string, code store.FailureCode, at time.Time) usernotify.RunRef {
	t.Helper()
	rs := f.srv.runs.RunStore()
	rctx := store.WithTenant(context.Background(), tenantID)
	if _, err := rs.CreateRun(rctx, runID, "feed-watch", nil); err != nil {
		t.Fatalf("create run %s: %v", runID, err)
	}
	r, err := rs.LoadRun(rctx, runID)
	if err != nil {
		t.Fatalf("load run %s: %v", runID, err)
	}
	r.TenantID = tenantID
	r.Status = status
	r.Error = errMsg
	r.FailureCode = code
	r.UpdatedAt = at
	if scheduleID != "" {
		r.Source = &store.RunSource{Kind: store.RunSourceKindSchedule, ScheduleID: scheduleID}
	}
	if err := rs.SaveRun(rctx, r); err != nil {
		t.Fatalf("save run %s: %v", runID, err)
	}
	return usernotify.RunRef{ID: runID, Status: string(status), UpdatedAt: at}
}

func (f *spineFixture) seedSchedule(t *testing.T, id, tenantID string) {
	t.Helper()
	if err := f.sched.Create(context.Background(), cloudsched.ScheduledBot{
		ID: id, TenantID: tenantID, BotID: "feed-watch", Cron: "* * * * *",
		NextFireAt: time.Now().Add(time.Minute),
		CreatedAt:  time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed schedule %s: %v", id, err)
	}
}

// TestRunOutcomeSpineSweepStampsASilentTerminal is the #1501 property the
// event-only subscriber #1426 shipped could not hold: a run that reached a
// terminal status WITHOUT any run-outcome event reaching a subscriber — a
// lossy bus, a replica gap, one of the terminal paths that publish nothing
// — is still stamped onto its record, because the store sweep is the
// authority and the event only the fast path.
//
// Nothing in this test publishes an event; the only writer is the sweep.
//
// Mutation (the one the unit's witness names): delete the store sweep —
// drop the `s.stampRunOutcome(ctx, ref.ID, cutoff)` call from
// runOutcomeSweepPass — and LastRunStatus stays empty: red on its own
// assertion.
func TestRunOutcomeSpineSweepStampsASilentTerminal(t *testing.T) {
	f := newSpineFixture(t)
	const scheduleID, tenantID = "sched-silent", "tenant-A"
	f.seedSchedule(t, scheduleID, tenantID)

	// Terminated 10 minutes ago: inside the sweep's lookback, outside its
	// grace, so the sweep owns it.
	now := time.Now().UTC()
	terminatedAt := now.Add(-10 * time.Minute)
	ref := f.seedRun(t, "run-silent", tenantID, scheduleID,
		store.RunStatusFailed, "boom", store.FailureSandboxDriverUnavailable, terminatedAt)
	f.listed = []usernotify.RunRef{ref}

	f.srv.runOutcomeSweepPass(context.Background(), now)

	got, err := f.sched.Get(context.Background(), scheduleID)
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if got.LastRunID != "run-silent" {
		t.Errorf("LastRunID=%q; want %q — the sweep did not stamp a terminal nobody announced", got.LastRunID, "run-silent")
	}
	if got.LastRunStatus != string(store.RunStatusFailed) {
		t.Errorf("LastRunStatus=%q; want %q", got.LastRunStatus, string(store.RunStatusFailed))
	}
	if got.LastRunError != "boom" {
		t.Errorf("LastRunError=%q; want %q", got.LastRunError, "boom")
	}
	if got.LastRunErrorCode != string(store.FailureSandboxDriverUnavailable) {
		t.Errorf("LastRunErrorCode=%q; want %q", got.LastRunErrorCode, string(store.FailureSandboxDriverUnavailable))
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(terminatedAt) {
		t.Errorf("LastRunAt=%v; want the run document's own instant %v", got.LastRunAt, terminatedAt)
	}
}

// TestRunOutcomeSpineSweepLeavesTheGraceWindowToTheEventPath pins that a
// run which terminated seconds ago is left alone by the sweep: the event
// path may still be writing it, and the grace is what keeps the two off
// the same row. The grace lives in ONE place — stampRunOutcome, the
// chokepoint both paths cross — so this is where it is witnessed.
//
// Mutation: drop the `run.UpdatedAt.After(updatedBefore)` guard from
// stampRunOutcome → the fresh terminal is stamped → red.
func TestRunOutcomeSpineSweepLeavesTheGraceWindowToTheEventPath(t *testing.T) {
	f := newSpineFixture(t)
	const scheduleID, tenantID = "sched-fresh", "tenant-A"
	f.seedSchedule(t, scheduleID, tenantID)

	now := time.Now().UTC()
	ref := f.seedRun(t, "run-fresh", tenantID, scheduleID,
		store.RunStatusFinished, "", "", now.Add(-5*time.Second))
	f.listed = []usernotify.RunRef{ref}

	f.srv.runOutcomeSweepPass(context.Background(), now)

	got, err := f.sched.Get(context.Background(), scheduleID)
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if got.LastRunID != "" {
		t.Errorf("LastRunID=%q; want empty — a terminal inside the grace belongs to the event path", got.LastRunID)
	}
}

// TestRunOutcomeSpineSweepJudgesTheListingBeforeLoadingTheRun pins the
// cheap pre-check whose whole value is a run load NOT done: the window scan
// also returns every run parked on a human gate, and those are not
// time-bounded, so a fleet's long-lived pauses come back on every pass.
//
// The fixture is the only one that can tell the pre-check apart from
// stampRunOutcome's own terminal gate: the stored run IS terminal while the
// listing row still says `paused_waiting_human` — the skew a projection
// read at scan time really produces. Judged on the listing, the row is
// skipped (and offered again by the next pass, 60 s later); judged on the
// document, it would be stamped.
//
// Mutation: drop the `!store.RunStatus(ref.Status).IsTerminal()` guard from
// runOutcomeSweepPass → the row is loaded, found terminal and stamped → red.
func TestRunOutcomeSpineSweepJudgesTheListingBeforeLoadingTheRun(t *testing.T) {
	f := newSpineFixture(t)
	const scheduleID, tenantID = "sched-stale-row", "tenant-A"
	f.seedSchedule(t, scheduleID, tenantID)

	now := time.Now().UTC()
	ref := f.seedRun(t, "run-stale-row", tenantID, scheduleID,
		store.RunStatusFinished, "", "", now.Add(-30*time.Minute))
	ref.Status = string(store.RunStatusPausedWaitingHuman)
	f.listed = []usernotify.RunRef{ref}

	f.srv.runOutcomeSweepPass(context.Background(), now)

	got, err := f.sched.Get(context.Background(), scheduleID)
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if got.LastRunID != "" {
		t.Errorf("LastRunID=%q; want empty — the sweep must judge the listing row, not load every parked run", got.LastRunID)
	}
}

// TestRunOutcomeSpineEventPathRefusesANonTerminalRun pins the OTHER
// terminal gate, the one on the chokepoint: the bus path has no listing to
// judge, and a run-outcome event can name a run that has already moved on
// (a resumed failure, a redelivery). Only a terminal run has an outcome to
// report.
//
// Mutation: drop the `!run.Status.IsTerminal()` gate from stampRunOutcome →
// the paused run's status is stamped onto the schedule → red.
func TestRunOutcomeSpineEventPathRefusesANonTerminalRun(t *testing.T) {
	f := newSpineFixture(t)
	const scheduleID, tenantID = "sched-paused", "tenant-A"
	f.seedSchedule(t, scheduleID, tenantID)

	now := time.Now().UTC()
	f.seedRun(t, "run-paused", tenantID, scheduleID,
		store.RunStatusPausedWaitingHuman, "", "", now.Add(-30*time.Minute))

	if err := f.srv.handleRunOutcomeEvent(context.Background(), trigger.Event{
		Source:  trigger.SourceRun,
		Kind:    trigger.KindRunFinished,
		Subject: trigger.Subject{Type: "run", ID: "run-paused"},
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	got, err := f.sched.Get(context.Background(), scheduleID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastRunID != "" {
		t.Errorf("LastRunID=%q; want empty — a paused run has no outcome to report yet", got.LastRunID)
	}
}

// TestRunOutcomeSpineSweepIsIdempotent pins that the spine needs no claim
// registry: a second pass over the same terminal converges on the same row
// instead of moving it, which is what lets every replica sweep at once.
//
// Mutation: drop the monotonicity guard from
// cloudsched.MemoryStore.MarkRunOutcome (the `LastRunAt.After(at)` early
// return) and the older outcome below overwrites the newer one → red on
// LastRunID.
func TestRunOutcomeSpineSweepIsIdempotent(t *testing.T) {
	f := newSpineFixture(t)
	const scheduleID, tenantID = "sched-twice", "tenant-A"
	f.seedSchedule(t, scheduleID, tenantID)

	now := time.Now().UTC()
	older := f.seedRun(t, "run-older", tenantID, scheduleID,
		store.RunStatusFailed, "first", "code_a", now.Add(-20*time.Minute))
	newer := f.seedRun(t, "run-newer", tenantID, scheduleID,
		store.RunStatusFinished, "", "", now.Add(-10*time.Minute))

	// Newest first, as the scan returns them — then the whole pass again.
	f.listed = []usernotify.RunRef{newer, older}
	f.srv.runOutcomeSweepPass(context.Background(), now)
	f.srv.runOutcomeSweepPass(context.Background(), now)

	got, err := f.sched.Get(context.Background(), scheduleID)
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if got.LastRunID != "run-newer" {
		t.Errorf("LastRunID=%q; want %q — a re-offered older terminal must not move the record backwards", got.LastRunID, "run-newer")
	}
	if got.LastRunError != "" {
		t.Errorf("LastRunError=%q; want empty — the newer successful outcome clears it", got.LastRunError)
	}
}

// TestRunOutcomeSpineSweepIsNotStartedWithoutAWindowScan pins the honest
// degradation: with no terminal-run window scan wired (self-hosted, or a
// store that cannot serve one) the spine keeps the event fast path and does
// NOT pretend to a sweep. The sweep pass is the thing under test, so it is
// called directly: without a lister it must do nothing rather than panic.
func TestRunOutcomeSpineSweepIsNotStartedWithoutAWindowScan(t *testing.T) {
	f := newSpineFixture(t)
	f.srv.cfg.NotifiableRuns = nil
	f.srv.runOutcomeSweepPass(context.Background(), time.Now())
	if f.calls != 0 {
		t.Errorf("listing calls=%d; want 0", f.calls)
	}
}

// TestRunOutcomeSinksAreEmptyWithoutARecordStore pins the no-op boot: a
// server wired with none of the record stores registers no sink, so the
// spine never subscribes and never sweeps.
func TestRunOutcomeSinksAreEmptyWithoutARecordStore(t *testing.T) {
	workDir := t.TempDir()
	srv := New(Config{
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelError, nil))
	if got := srv.runOutcomeSinks(); len(got) != 0 {
		t.Errorf("runOutcomeSinks()=%d sinks; want 0 without a record store", len(got))
	}
}

// TestRunOutcomeSpineStampsTheTriggerSubscription is the #1501 property for
// trigger.Subscription: a subscription that fired and whose run died must
// no longer read like one that fired and worked. The subscription's own
// LastError only ever covered the LAUNCH, so the failure below is invisible
// without the outcome tuple.
//
// Mutation: drop the trigger sink from runOutcomeSinks → LastRunStatus
// stays empty → red.
func TestRunOutcomeSpineStampsTheTriggerSubscription(t *testing.T) {
	f := newSpineFixture(t)
	subs := trigger.NewMemorySubscriptionStore()
	f.srv.cfg.TriggerStore = subs
	const subID, tenantID = "sub-1", "tenant-A"
	if err := subs.Create(context.Background(), trigger.Subscription{
		ID: subID, TenantID: tenantID, BotID: "feed-watch", Enabled: true,
		// A launch that went through: LastError is empty, which today is
		// the whole story the record can tell.
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	now := time.Now().UTC()
	terminatedAt := now.Add(-10 * time.Minute)
	f.seedRun(t, "run-trigger", tenantID, subID,
		store.RunStatusFailed, "bot exited 1", store.FailureCode("BOT_EXIT"), terminatedAt)

	if err := f.srv.handleRunOutcomeEvent(context.Background(), trigger.Event{
		Source:  trigger.SourceRun,
		Kind:    trigger.KindRunFailed,
		Subject: trigger.Subject{Type: "run", ID: "run-trigger"},
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	got, err := subs.Get(context.Background(), subID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	if got.LastRunID != "run-trigger" || got.LastRunStatus != string(store.RunStatusFailed) {
		t.Errorf("outcome = (%q, %q); want (run-trigger, failed)", got.LastRunID, got.LastRunStatus)
	}
	if got.LastRunError != "bot exited 1" || got.LastRunErrorCode != "BOT_EXIT" {
		t.Errorf("outcome error = (%q, %q); want (bot exited 1, BOT_EXIT)", got.LastRunError, got.LastRunErrorCode)
	}
	if got.LastError != "" {
		t.Errorf("LastError=%q; want empty — the LAUNCH went through, and the run's failure is a different field", got.LastError)
	}
}

// TestRunOutcomeSpineStampsTheBoardCard is the #1501 property for the
// dispatcher card: before it, the only reader of a dispatched card's run
// was the reaper's claim poller, so a card whose run died read as
// dispatched.
//
// Mutation: drop the board sink from runOutcomeSinks → LastRunStatus stays
// empty → red.
func TestRunOutcomeSpineStampsTheBoardCard(t *testing.T) {
	f := newSpineFixture(t)
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = board.Close() })
	const tenantID = "tenant-A"
	f.srv.cfg.CloudBoardFor = func(tenant string) native.BoardStore {
		if tenant != tenantID {
			return nil
		}
		return board
	}
	iss, err := board.Create(native.Issue{Title: "dispatched card", State: native.StateReady})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := board.SetLastRun(iss.ID, "run-card", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}

	now := time.Now().UTC()
	rs := f.srv.runs.RunStore()
	rctx := store.WithTenant(context.Background(), tenantID)
	if _, err := rs.CreateRun(rctx, "run-card", "fixer", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	r, err := rs.LoadRun(rctx, "run-card")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	r.TenantID = tenantID
	r.Status = store.RunStatusFailed
	r.Error = "fixer gave up"
	r.FailureCode = store.FailureCode("GAVE_UP")
	r.UpdatedAt = now.Add(-10 * time.Minute)
	r.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: iss.ID}
	if err := rs.SaveRun(rctx, r); err != nil {
		t.Fatalf("save run: %v", err)
	}

	if err := f.srv.handleRunOutcomeEvent(context.Background(), trigger.Event{
		Source:  trigger.SourceRun,
		Kind:    trigger.KindRunFailed,
		Subject: trigger.Subject{Type: "run", ID: "run-card"},
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	got, err := board.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get card: %v", err)
	}
	if got.LastRunStatus != string(store.RunStatusFailed) {
		t.Errorf("LastRunStatus=%q; want %q — the card still reads as merely dispatched", got.LastRunStatus, store.RunStatusFailed)
	}
	if got.LastRunError != "fixer gave up" || got.LastRunErrorCode != "GAVE_UP" {
		t.Errorf("outcome error = (%q, %q); want (fixer gave up, GAVE_UP)", got.LastRunError, got.LastRunErrorCode)
	}
}

// TestRunOutcomeSpineCadenceSinksDoNotCrossRecords pins the consequence of
// store.RunSource.ScheduleID carrying TWO id spaces (a cloudsched schedule
// and a trigger subscription): both cadence sinks are offered every such
// run, and the one that does not own the record must answer silently rather
// than report an outage — a trigger-fired run must not leave the schedule
// store looking broken, and must not stamp a schedule that happens to share
// the id either.
//
// Mutation: drop the `errors.Is(err, trigger.ErrSubscriptionNotFound)` /
// `cloudsched.ErrNotFound` mapping and the offer returns the sink's error,
// which the spine logs as a store failure; drop the sink's Get-by-id
// contract instead and the foreign record below is stamped → red.
func TestRunOutcomeSpineCadenceSinksDoNotCrossRecords(t *testing.T) {
	f := newSpineFixture(t)
	subs := trigger.NewMemorySubscriptionStore()
	f.srv.cfg.TriggerStore = subs
	const tenantID = "tenant-A"
	// A schedule and a subscription with DIFFERENT ids: the run names the
	// subscription, so the schedule must stay untouched.
	f.seedSchedule(t, "sched-other", tenantID)
	if err := subs.Create(context.Background(), trigger.Subscription{
		ID: "sub-owner", TenantID: tenantID, BotID: "feed-watch", Enabled: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	now := time.Now().UTC()
	f.seedRun(t, "run-sub", tenantID, "sub-owner",
		store.RunStatusFinished, "", "", now.Add(-10*time.Minute))

	if err := f.srv.handleRunOutcomeEvent(context.Background(), trigger.Event{
		Source:  trigger.SourceRun,
		Kind:    trigger.KindRunFinished,
		Subject: trigger.Subject{Type: "run", ID: "run-sub"},
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	sub, err := subs.Get(context.Background(), "sub-owner")
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	if sub.LastRunID != "run-sub" {
		t.Errorf("subscription LastRunID=%q; want run-sub", sub.LastRunID)
	}
	sched, err := f.sched.Get(context.Background(), "sched-other")
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if sched.LastRunID != "" {
		t.Errorf("schedule LastRunID=%q; want empty — the run named a subscription, not this schedule", sched.LastRunID)
	}
}

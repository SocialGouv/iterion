package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/retryprobetest"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// The credential probe (#3127): runs parked on one credential's usage window
// are probed ONCE per floor — one of them relaunched as the canary — and
// released together on its answer. Driven over the sweeper's REAL passes (the
// due pass and the probe pass, minute by minute) on a fleet of the store twin
// (internal/retryprobetest), each run's row mirroring the Mongo update
// documents the passes write; the runner's side is the two writes it makes
// after a probed resume: the free re-arm of a run that met the wall again
// (RearmProbedRunRetry on the provider's announced instant, as
// pkg/runner.armUsageWindowRetry does for a probe in flight), or a run that
// simply runs.

// credFleet is N runs, each its own twin row, behind the store and the lister
// the sweeper reads — the scans answer what Mongo's filters answer, row by
// row, in the order the fleet lists them.
type credFleet struct {
	store.RunStore

	mu    sync.Mutex
	now   time.Time
	runs  map[string]*retryprobetest.Store
	order []string
	// pulled records each landed pull with the instant of the pass that took
	// it, which is what the per-hour and same-pass assertions read.
	pulled []fleetPull
}

type fleetPull struct {
	runID string
	at    time.Time
}

func (p fleetPull) String() string { return p.runID + "@" + p.at.Format("15:04") }

func newCredFleet(start time.Time) *credFleet {
	return &credFleet{now: start, runs: map[string]*retryprobetest.Store{}}
}

func (f *credFleet) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *credFleet) setNow(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}

// park adds a run parked on a usage window dated `wall`, met on `creds` —
// the run's sealed credentials, which its paid arming copied into the wait
// (RunRetryState.WallCredentials) — whose last paid park was at `parkedAt`.
func (f *credFleet) park(id string, creds []string, parkedAt, wall time.Time) {
	retryAfter := wall.Add(time.Minute).Truncate(time.Millisecond)
	w, p := wall.Truncate(time.Millisecond), parkedAt.Truncate(time.Millisecond)
	tw := retryprobetest.New(&store.Run{
		ID:                id,
		TenantID:          "team-1",
		OwnerID:           "user-1",
		FilePath:          "bots/feed-watch/main.bot",
		Status:            store.RunStatusFailedResumable,
		FailureCode:       store.FailureUsageLimitBlocked,
		ContinuationState: store.ContinuationRetryArmed,
		CredFingerprints:  creds,
		RetryPolicy:       &store.RunRetryPolicy{MaxAttempts: 5, MaxWait: "192h", Jitter: "0s"},
		RetryState: &store.RunRetryState{
			RetryAfter:  &retryAfter,
			ScheduledAt: &p,
			Wall:        &w,
			Reason:      store.RetryReasonUsageWindow,
			Code:        "USAGE_LIMIT_BLOCKED",
			Attempts:    1,
			// What mongo's ScheduleRunRetry copies off the document in the
			// arming's own update.
			WallCredentials: slices.Clone(creds),
		},
	})
	tw.Clock = f.clock
	f.runs[id] = tw
	f.order = append(f.order, id)
}

func (f *credFleet) twin(id string) *retryprobetest.Store {
	tw, ok := f.runs[id]
	if !ok {
		panic("credential fleet: no run " + id)
	}
	return tw
}

func refOfRun(r *store.Run) mongostore.RetryDueRef {
	return mongostore.RetryDueRef{
		ID: r.ID, TenantID: r.TenantID, OwnerID: r.OwnerID, BotID: r.BotID, FilePath: r.FilePath,
		BotSourceTenant: r.BotSourceTenant, RetryState: r.RetryState,
		Status: r.Status, FailureCode: r.FailureCode, ContinuationState: r.ContinuationState,
		SkippedCredReopensAt: r.SkippedCredReopensAt, RetryPolicy: r.RetryPolicy,
	}
}

func (f *credFleet) ListRunsDueForRetry(_ context.Context, before time.Time, limit int) ([]mongostore.RetryDueRef, error) {
	var out []mongostore.RetryDueRef
	for _, id := range f.order {
		if len(f.runs[id].DueRows(before, limit)) > 0 {
			out = append(out, refOfRun(f.runs[id].Doc()))
		}
	}
	slices.SortStableFunc(out, func(a, b mongostore.RetryDueRef) int { return a.RetryAfter().Compare(b.RetryAfter()) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *credFleet) ListRunsForRetryProbe(_ context.Context, now time.Time, floor time.Duration, maxProbes, limit int) ([]mongostore.RetryDueRef, error) {
	var out []mongostore.RetryDueRef
	for _, id := range f.order {
		if len(f.runs[id].ProbeRows(now, floor, maxProbes, limit)) > 0 {
			out = append(out, refOfRun(f.runs[id].Doc()))
		}
	}
	return out, nil
}

// ListRunsForCredentialProbe answers in the fleet's own order, NOT the
// store's _id order: the pass's choice must not depend on it.
func (f *credFleet) ListRunsForCredentialProbe(_ context.Context, since time.Time, limit int) ([]mongostore.RetryDueRef, error) {
	var out []mongostore.RetryDueRef
	for _, id := range f.order {
		for _, r := range f.runs[id].CredentialProbeRows(since, limit) {
			out = append(out, refOfRun(r))
		}
	}
	return out, nil
}

func (f *credFleet) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	return f.twin(id).LoadRun(ctx, id)
}

func (f *credFleet) PullRunRetry(ctx context.Context, id string, expectedAfter, probeAt time.Time) (bool, error) {
	ok, err := f.twin(id).PullRunRetry(ctx, id, expectedAfter, probeAt)
	if ok {
		f.mu.Lock()
		f.pulled = append(f.pulled, fleetPull{runID: id, at: f.now})
		f.mu.Unlock()
	}
	return ok, err
}

func (f *credFleet) ScheduleRunRetry(ctx context.Context, id string, at, wall time.Time, reason, code string, maxAttempts int) (bool, int, error) {
	return f.twin(id).ScheduleRunRetry(ctx, id, at, wall, reason, code, maxAttempts)
}

func (f *credFleet) DelayRunRetry(ctx context.Context, id string, expectedAfter, delayedUntil time.Time, reason, code string) (bool, error) {
	return f.twin(id).DelayRunRetry(ctx, id, expectedAfter, delayedUntil, reason, code)
}

func (f *credFleet) RearmProbedRunRetry(ctx context.Context, id string, at, wall time.Time) (bool, error) {
	return f.twin(id).RearmProbedRunRetry(ctx, id, at, wall)
}

func (f *credFleet) ClaimRunRetry(ctx context.Context, id string, expectedAfter time.Time) (bool, error) {
	return f.twin(id).ClaimRunRetry(ctx, id, expectedAfter)
}

func (f *credFleet) ClearRunRetry(ctx context.Context, id string) error {
	return f.twin(id).ClearRunRetry(ctx, id)
}

func (f *credFleet) AbandonRunRetry(ctx context.Context, id, reason string, expectedAfter time.Time) (bool, error) {
	return f.twin(id).AbandonRunRetry(ctx, id, reason, expectedAfter)
}

func (f *credFleet) RecordRetryFailure(context.Context, string, string, time.Time, int, time.Duration) (*store.RetryCircuitState, error) {
	return nil, nil
}

func (f *credFleet) RetryCircuitOpen(context.Context, string, time.Time) (*store.RetryCircuitState, error) {
	return nil, nil
}

func (f *credFleet) RecordRetrySuccess(context.Context, string, time.Time) error { return nil }

// pullsAt is the runs pulled by the pass at t.
func (f *credFleet) pullsAt(t time.Time) []string {
	var ids []string
	for _, p := range f.pulled {
		if p.at.Equal(t) {
			ids = append(ids, p.runID)
		}
	}
	return ids
}

// credFleetResumer is the run service's half of a resume: the run leaves
// failed_resumable for queued, which is what SubmitResume's CAS writes, and
// its sealed credentials are re-stamped from the resume's own re-resolution
// (cloudpublisher's SetRunCredStamp, unconditional at every resume). The
// re-resolution seals restamp(run) — by default nothing (an empty
// resolution clears the stamp), never the set the run parked on: a window
// that kept its runs only while the stamp held still would pass here.
type credFleetResumer struct {
	f       *credFleet
	mu      sync.Mutex
	calls   []fleetPull
	restamp func(runID string) []string
}

func (r *credFleetResumer) Resume(_ context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
	var sealed []string
	if r.restamp != nil {
		sealed = r.restamp(spec.RunID)
	}
	r.f.twin(spec.RunID).Mutate(func(run *store.Run) {
		run.Status = store.RunStatusQueued
		run.CredFingerprints = sealed
	})
	r.mu.Lock()
	r.calls = append(r.calls, fleetPull{runID: spec.RunID, at: r.f.clock()})
	r.mu.Unlock()
	return &runview.LaunchResult{RunID: spec.RunID}, nil
}

// meetsTheWallAgain is the runner's park of a probed resume that the
// provider refused again: the engine's failed_resumable on
// USAGE_LIMIT_BLOCKED, then the free re-arm on the instant the probe held
// aside, with no news about the wall.
func (f *credFleet) meetsTheWallAgain(t *testing.T, id string) {
	t.Helper()
	tw := f.twin(id)
	tw.Mutate(func(run *store.Run) {
		run.Status = store.RunStatusFailedResumable
		run.FailureCode = store.FailureUsageLimitBlocked
	})
	announced := tw.Row().ProbeFrom
	if announced == nil {
		t.Fatalf("run %s met the wall with no probe marker — the fixture expected a probed resume", id)
	}
	if ok, err := tw.RearmProbedRunRetry(context.Background(), id, *announced, time.Time{}); err != nil || !ok {
		t.Fatalf("free re-arm of %s = (%v, %v)", id, ok, err)
	}
}

// credWorld is the server, the fleet and the clock, ticked a minute at a time.
type credWorld struct {
	t       *testing.T
	f       *credFleet
	s       *Server
	resumer *credFleetResumer
	start   time.Time
	// onResume is the runner's answer to each resume. It runs in the tick
	// that resumed the run and may defer its writes with later: a resumed
	// pod reaches the provider minutes after the due pass, and the probe
	// passes in between see the run in flight.
	onResume func(id string)
	pending  []fleetAction
}

type fleetAction struct {
	at time.Time
	do func()
}

// later applies do in the first tick at least `after` past the current one.
func (w *credWorld) later(after time.Duration, do func()) {
	w.pending = append(w.pending, fleetAction{at: w.f.clock().Add(after), do: do})
}

// runnerAnswers is how long a resumed pod takes to meet the provider: past
// the due pass, short of store.ProbeMarkerTTL.
const runnerAnswers = 5 * time.Minute

func newCredWorld(t *testing.T) *credWorld {
	t.Helper()
	// The passes read the wall clock in places (the due pass's window hold,
	// the payment question), so the simulated clock starts at it.
	start := time.Now().UTC().Truncate(time.Minute)
	f := newCredFleet(start)
	return &credWorld{t: t, f: f, s: newRetrySweeperServer(t, f, nil), resumer: &credFleetResumer{f: f}, start: start}
}

// tick runs one sweeper tick at start+offset: the due pass, the runner's
// answer to what it resumed, then the probe pass — runRetrySweeper's order.
func (w *credWorld) tick(offset time.Duration) {
	now := w.start.Add(offset)
	w.f.setNow(now)
	before := len(w.resumer.calls)
	w.s.sweepDueRetries(context.Background(), w.f, w.resumer, now)
	if w.onResume != nil {
		for _, c := range w.resumer.calls[before:] {
			w.onResume(c.runID)
		}
	}
	var keep []fleetAction
	for _, a := range w.pending {
		if a.at.After(now) {
			keep = append(keep, a)
			continue
		}
		a.do()
	}
	w.pending = keep
	w.s.sweepRetryProbes(context.Background(), w.f, now)
}

func (w *credWorld) run(from, to time.Duration) {
	for off := from; off <= to; off += time.Minute {
		w.tick(off)
	}
}

// N runs parked on one credential window: the window is probed once per
// floor — one relaunch an hour — by the wait pulled least recently, the
// earliest-parked first, while the others are neither pulled nor resumed.
func TestCredentialProbe_NRunsOnOneCredentialRelaunchOneRunAnHour(t *testing.T) {
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	for i, id := range []string{"run-e", "run-b", "run-a", "run-d", "run-c"} {
		w.f.park(id, []string{"fp-zai-weekly"}, w.start.Add(-time.Duration(10-i)*time.Minute), wall)
	}
	w.onResume = func(id string) { w.later(runnerAnswers, func() { w.f.meetsTheWallAgain(t, id) }) }

	w.run(0, 3*time.Hour-time.Minute)

	perHour := map[int][]string{}
	for _, c := range w.resumer.calls {
		h := int(c.at.Sub(w.start) / time.Hour)
		perHour[h] = append(perHour[h], c.runID)
	}
	// Parked at -10m, -9m, -8m…: never-pulled waits go first, earliest-parked
	// first, so the canary walks the window.
	for h, want := range []string{"run-e", "run-b", "run-a"} {
		if got := perHour[h]; len(got) != 1 || got[0] != want {
			t.Fatalf("hour %d relaunched %v, want exactly one relaunch, of %s — five runs share one credential window and one probe answers for all of them\nall resumes: %v",
				h, got, want, w.resumer.calls)
		}
	}
	if len(w.f.pulled) != 3 {
		t.Fatalf("pulls = %v, want the three canaries only — nobody else moves while the wall stands", w.f.pulled)
	}
}

// The canary meets the wall again: nobody else moves, and the window is not
// probed again before a floor has passed since that pull.
func TestCredentialProbe_ACanaryThatMeetsTheWallMovesNobodyForAnHour(t *testing.T) {
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("run-a", []string{"fp-zai-weekly"}, w.start.Add(-30*time.Minute), wall)
	w.f.park("run-b", []string{"fp-zai-weekly"}, w.start.Add(-20*time.Minute), wall)
	w.onResume = func(id string) { w.later(runnerAnswers, func() { w.f.meetsTheWallAgain(t, id) }) }

	w.run(0, 59*time.Minute)
	if len(w.f.pulled) != 1 || w.f.pulled[0].runID != "run-a" || !w.f.pulled[0].at.Equal(w.start) {
		t.Fatalf("pulls in the first hour = %v, want run-a alone at %s — the canary met the wall again, and its pull paces the window for an hour",
			w.f.pulled, w.start.Format(time.RFC3339))
	}
	if row := w.f.twin("run-b").Row(); row.ProbeFrom != nil || row.LastProbeAt != nil || !row.RetryAfter.Equal(wall.Add(time.Minute).Truncate(time.Millisecond)) {
		t.Fatalf("run-b's row = %+v, want untouched on its announced instant — a canary refused again releases nobody", row)
	}
	w.tick(time.Hour)
	if got := w.f.pullsAt(w.start.Add(time.Hour)); len(got) != 1 || got[0] != "run-b" {
		t.Fatalf("pulls at +1h = %v, want one canary one floor after the last pull — run-b, the wait not pulled yet", got)
	}
}

// The canary got past the wall — it has run longer than a refused resume
// takes to come back: every other run parked on the same credentials is
// released in the same pass.
func TestCredentialProbe_ACanaryPastTheWallReleasesTheOthersInOnePass(t *testing.T) {
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	for i, id := range []string{"run-a", "run-b", "run-c", "run-d"} {
		w.f.park(id, []string{"fp-zai-weekly"}, w.start.Add(-time.Duration(40-i)*time.Minute), wall)
	}
	// The runner claims the canary and works: the wall fell (a hand top-up).
	w.onResume = func(id string) {
		w.f.twin(id).Mutate(func(run *store.Run) { run.Status = store.RunStatusRunning })
	}

	w.run(0, store.ProbeMarkerTTL-time.Minute)
	if len(w.f.pulled) != 1 {
		t.Fatalf("pulls before the canary's answer = %v, want the canary alone — a probe still out is not an answer", w.f.pulled)
	}
	answer := store.ProbeMarkerTTL
	w.tick(answer)
	released := w.f.pullsAt(w.start.Add(answer))
	slices.Sort(released)
	if want := []string{"run-b", "run-c", "run-d"}; !slices.Equal(released, want) {
		t.Fatalf("released at +%s = %v, want %v in that one pass — the canary has run past the wall, so the window it waited on is open for every run parked on it",
			answer, released, want)
	}
	w.onResume = nil
	w.tick(answer + time.Minute)
	resumed := map[string]bool{}
	for _, c := range w.resumer.calls {
		resumed[c.runID] = true
	}
	for _, id := range []string{"run-b", "run-c", "run-d"} {
		if !resumed[id] {
			t.Fatalf("run %s was released and not resumed by the next due pass: %v", id, w.resumer.calls)
		}
	}
}

// A canary that FINISHED is past the wall too, however soon: the release does
// not wait for the marker's age.
func TestCredentialProbe_ACanaryThatFinishedReleasesTheOthersAtOnce(t *testing.T) {
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("run-a", []string{"fp-zai-weekly"}, w.start.Add(-30*time.Minute), wall)
	w.f.park("run-b", []string{"fp-zai-weekly"}, w.start.Add(-20*time.Minute), wall)
	w.onResume = func(id string) {
		w.f.twin(id).Mutate(func(run *store.Run) { run.Status = store.RunStatusFinished })
	}
	w.run(0, 2*time.Minute)
	if got := w.f.pullsAt(w.start.Add(time.Minute)); !slices.Equal(got, []string{"run-b"}) {
		t.Fatalf("pulls at +1m = %v (all pulls %v), want run-b released in the pass that read the finished canary", got, w.f.pulled)
	}
}

// Two credentials, two windows: one canary each, in the same pass.
func TestCredentialProbe_TwoCredentialsOneCanaryEach(t *testing.T) {
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("zai-1", []string{"fp-zai"}, w.start.Add(-50*time.Minute), wall)
	w.f.park("zai-2", []string{"fp-zai"}, w.start.Add(-40*time.Minute), wall)
	w.f.park("zai-3", []string{"fp-zai"}, w.start.Add(-30*time.Minute), wall)
	w.f.park("kimi-1", []string{"fp-kimi"}, w.start.Add(-20*time.Minute), wall)
	w.f.park("kimi-2", []string{"fp-kimi"}, w.start.Add(-10*time.Minute), wall)

	w.tick(0)
	got := w.f.pullsAt(w.start)
	slices.Sort(got)
	if want := []string{"kimi-1", "zai-1"}; !slices.Equal(got, want) {
		t.Fatalf("first pass pulled %v, want %v — one canary per credential window", got, want)
	}
}

// The canary is the earliest-parked run, whatever order the scan lists the
// rows in: two replicas, or one restarted, pick the same run.
func TestCredentialProbe_TheCanaryIsTheSameInBothOrdersOfTheRows(t *testing.T) {
	pick := func(reverse bool) string {
		w := newCredWorld(t)
		wall := w.start.Add(4 * 24 * time.Hour)
		// Parked order differs from both the id order and the listing order.
		w.f.park("run-a", []string{"fp-zai"}, w.start.Add(-10*time.Minute), wall)
		w.f.park("run-c", []string{"fp-zai"}, w.start.Add(-30*time.Minute), wall)
		w.f.park("run-b", []string{"fp-zai"}, w.start.Add(-20*time.Minute), wall)
		if reverse {
			slices.Reverse(w.f.order)
		}
		w.tick(0)
		if len(w.f.pulled) != 1 {
			t.Fatalf("reverse=%v: pulls = %v, want one canary", reverse, w.f.pulled)
		}
		return w.f.pulled[0].runID
	}
	forward, backward := pick(false), pick(true)
	if forward != backward || forward != "run-c" {
		t.Fatalf("canary = %s listed forward and %s listed backward, want run-c (the earliest-parked) both times", forward, backward)
	}
}

// A canary's answer releases its own window only: a run parked on other
// credentials — even on the same dated wall — stays where it is.
func TestCredentialProbe_ACanaryNeverReleasesAnotherCredentialsRun(t *testing.T) {
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("zai-1", []string{"fp-zai"}, w.start.Add(-50*time.Minute), wall)
	w.f.park("zai-2", []string{"fp-zai"}, w.start.Add(-40*time.Minute), wall)
	w.f.park("kimi-1", []string{"fp-kimi"}, w.start.Add(-30*time.Minute), wall)
	w.f.park("kimi-2", []string{"fp-kimi"}, w.start.Add(-20*time.Minute), wall)
	w.onResume = func(id string) {
		if strings.HasPrefix(id, "zai") {
			w.f.twin(id).Mutate(func(run *store.Run) { run.Status = store.RunStatusFinished })
			return
		}
		w.later(runnerAnswers, func() { w.f.meetsTheWallAgain(t, id) })
	}
	w.run(0, 2*runnerAnswers)
	for _, p := range w.f.pulled {
		if p.runID == "kimi-2" {
			t.Fatalf("kimi-2 was pulled at %s — the zai canary's answer says nothing about the kimi credential's wall\nall pulls: %v", p.at.Format(time.RFC3339), w.f.pulled)
		}
	}
	if got := w.f.pullsAt(w.start.Add(time.Minute)); !slices.Equal(got, []string{"zai-2"}) {
		t.Fatalf("pulls at +1m = %v, want zai-2 alone, released by its own window's canary", got)
	}
}

// A wait that met the wall AFTER the canary was pulled carries newer news
// than the canary: it is not released on that answer.
func TestCredentialProbe_AWaitParkedAfterThePullIsNotReleasedByIt(t *testing.T) {
	// A floor longer than the answer takes, so the pass that reads the
	// canary's answer takes no new probe of the window: a pull of run-late
	// there could only be the release.
	t.Setenv(EnvRetryProbeFloor, "2h")
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("run-a", []string{"fp-zai"}, w.start.Add(-30*time.Minute), wall)
	w.onResume = func(id string) {
		w.f.twin(id).Mutate(func(run *store.Run) { run.Status = store.RunStatusRunning })
	}
	w.run(0, 10*time.Minute)
	w.f.park("run-late", []string{"fp-zai"}, w.start.Add(10*time.Minute), wall)
	w.run(11*time.Minute, store.ProbeMarkerTTL+time.Minute)
	for _, p := range w.f.pulled {
		if p.runID == "run-late" {
			t.Fatalf("run-late, parked at +10m on the wall, was pulled at %s on the answer of a probe pulled at +0", p.at.Format(time.RFC3339))
		}
	}
}

// The notice names the date, and says the credential is probed every floor
// until then — the one relaunch that may come before the date.
func TestCredentialProbe_TheNoticeSaysTheDateAndTheHourlyProbe(t *testing.T) {
	now := time.Now().UTC()
	wall := now.Add(4 * 24 * time.Hour).Truncate(time.Minute)
	retryAfter := wall.Add(time.Minute)
	run := func(creds ...string) *store.Run {
		return &store.Run{
			ID: "bko-82", Status: store.RunStatusFailedResumable, FailureCode: store.FailureUsageLimitBlocked,
			CredFingerprints: creds,
			RetryState: &store.RunRetryState{
				RetryAfter: &retryAfter, Wall: &wall, ScheduledAt: &now,
				Reason: store.RetryReasonUsageWindow, Attempts: 1,
				WallCredentials: creds,
			},
		}
	}
	date := retryAfter.Format("15:04 UTC on 2006-01-02")
	body := gatePauseNoticeBody(run("fp-zai"), pauseNoticeRoleReviewer, now)
	if !strings.Contains(body, date) || !strings.Contains(body, "probes this credential's quota every hour") {
		t.Fatalf("notice = %q, want the date %s and the hourly probe of the credential", body, date)
	}
	// The probe stops at its budget, days before a weekly wall: the notice is
	// posted once, so it states the bound it cannot correct later.
	if !strings.Contains(body, "every hour, at most 24 times") {
		t.Fatalf("notice = %q, want the probe's bound (the default budget of 24) beside its cadence", body)
	}
	t.Setenv(EnvRetryProbeBudget, "3")
	if body := gatePauseNoticeBody(run("fp-zai"), pauseNoticeRoleReviewer, now); !strings.Contains(body, "every hour, at most 3 times") {
		t.Fatalf("notice = %q under a budget of 3, want that bound", body)
	}
	if body := gatePauseNoticeBody(run(), pauseNoticeRoleReviewer, now); strings.Contains(body, "every hour") {
		t.Fatalf("notice = %q for a run naming no credential — nothing probes it before its date, and the notice must not promise it", body)
	}
}

// The resume re-stamps the canary's sealed credentials (SetRunCredStamp,
// unconditional at every resume): an empty re-resolution, or the next pool
// slot. The canary meets the wall again and stays in the window it was
// pulled for — the window is the credentials its wall was met on, copied by
// the paid arming — so the window keeps its pace: one relaunch an hour, not
// one per run in the first minutes.
func TestCredentialProbe_ACanaryWhoseResumeReStampsItsCredentialsStaysInItsWindow(t *testing.T) {
	for _, restamp := range [][]string{nil, {"fp-zai-slot-2"}} {
		w := newCredWorld(t)
		wall := w.start.Add(4 * 24 * time.Hour)
		for i, id := range []string{"run-a", "run-b", "run-c", "run-d"} {
			w.f.park(id, []string{"fp-zai"}, w.start.Add(-time.Duration(40-i)*time.Minute), wall)
		}
		w.resumer.restamp = func(string) []string { return restamp }
		w.onResume = func(id string) { w.later(runnerAnswers, func() { w.f.meetsTheWallAgain(t, id) }) }

		w.run(0, 59*time.Minute)
		if len(w.f.pulled) != 1 || len(w.resumer.calls) != 1 {
			t.Fatalf("restamp=%v: first hour pulled %v and resumed %v, want one canary — one relaunch per credential window per hour",
				restamp, w.f.pulled, w.resumer.calls)
		}
		w.tick(time.Hour)
		if len(w.f.pulled) != 2 {
			t.Fatalf("restamp=%v: pulls by +1h = %v, want the window probed again one floor later", restamp, w.f.pulled)
		}
	}
}

// The canary finished; the runs it released meet the wall again (the top-up
// was small). A run released on an answer is not released again on the same
// answer: pass after pass, it would be a relaunch loop into the wall.
func TestCredentialProbe_ARunReleasedOnAnAnswerIsNotReleasedAgainOnIt(t *testing.T) {
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("run-a", []string{"fp-zai"}, w.start.Add(-30*time.Minute), wall)
	w.f.park("run-b", []string{"fp-zai"}, w.start.Add(-20*time.Minute), wall)
	w.f.park("run-c", []string{"fp-zai"}, w.start.Add(-10*time.Minute), wall)
	w.onResume = func(id string) {
		if id == "run-a" {
			w.f.twin(id).Mutate(func(run *store.Run) { run.Status = store.RunStatusFinished })
			return
		}
		w.later(2*time.Minute, func() { w.f.meetsTheWallAgain(t, id) })
	}
	w.run(0, 50*time.Minute)
	n := map[string]int{}
	for _, p := range w.f.pulled {
		n[p.runID]++
	}
	if n["run-b"] != 1 || n["run-c"] != 1 {
		t.Fatalf("pulls within 50 minutes = %v (%v), want run-b and run-c released once each on run-a's answer", n, w.f.pulled)
	}
}

// The window's probe budget bounds its probes: with a budget of two, two
// pulls, however long the wall stands — summed over the rows the canary
// rotated through.
func TestCredentialProbe_AWindowStopsAtItsProbeBudget(t *testing.T) {
	t.Setenv(EnvRetryProbeBudget, "2")
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("run-a", []string{"fp-zai"}, w.start.Add(-30*time.Minute), wall)
	w.f.park("run-b", []string{"fp-zai"}, w.start.Add(-20*time.Minute), wall)
	w.onResume = func(id string) { w.later(runnerAnswers, func() { w.f.meetsTheWallAgain(t, id) }) }
	w.run(0, 5*time.Hour)
	if len(w.f.pulled) != 2 {
		t.Fatalf("pulls over 5h with a probe budget of 2 = %v, want 2", w.f.pulled)
	}
}

// failingResumer refuses one run's resume before it reaches the provider (a
// transient publish refusal, its tenant's quota) and serves the others.
type failingResumer struct {
	inner *credFleetResumer
	fail  string
}

func (r *failingResumer) Resume(ctx context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
	if spec.RunID == r.fail {
		return nil, errors.New("publish refused (transient)")
	}
	return r.inner.Resume(ctx, spec)
}

// The earliest-parked canary's resume never reaches the provider. The window
// hands its next probe to another run instead of spending its whole budget
// asking nobody — which would leave a top-up unnoticed for the whole wall.
func TestCredentialProbe_ACanaryThatNeverReachesTheProviderHandsTheNextProbeOn(t *testing.T) {
	t.Setenv(EnvRetryProbeBudget, "4")
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("run-a", []string{"fp-platform"}, w.start.Add(-30*time.Minute), wall)
	w.f.park("run-b", []string{"fp-platform"}, w.start.Add(-20*time.Minute), wall)
	w.f.park("run-c", []string{"fp-platform"}, w.start.Add(-10*time.Minute), wall)
	res := &failingResumer{inner: w.resumer, fail: "run-a"}
	for off := time.Duration(0); off <= 6*time.Hour; off += time.Minute {
		now := w.start.Add(off)
		w.f.setNow(now)
		w.s.sweepDueRetries(context.Background(), w.f, res, now)
		w.s.sweepRetryProbes(context.Background(), w.f, now)
	}
	got := map[string]int{}
	for _, p := range w.f.pulled {
		got[p.runID]++
	}
	if got["run-b"] == 0 || got["run-c"] == 0 {
		t.Fatalf("pulls over 6h = %v (%v), want run-b and run-c asked too — run-a's resume never reaches the provider", got, w.f.pulled)
	}
}

// Two replicas read the same records at the same instants: one canary per
// window per floor between them.
func TestCredentialProbe_TwoReplicasPullOneCanary(t *testing.T) {
	w := newCredWorld(t)
	second := newRetrySweeperServer(t, w.f, nil)
	wall := w.start.Add(4 * 24 * time.Hour)
	for i, id := range []string{"run-a", "run-b", "run-c"} {
		w.f.park(id, []string{"fp-zai"}, w.start.Add(-time.Duration(40-i)*time.Minute), wall)
	}
	w.onResume = func(id string) { w.later(runnerAnswers, func() { w.f.meetsTheWallAgain(t, id) }) }
	for off := time.Duration(0); off < 2*time.Hour; off += time.Minute {
		w.tick(off)
		second.sweepRetryProbes(context.Background(), w.f, w.start.Add(off).Add(20*time.Second))
	}
	if len(w.f.pulled) != 2 {
		t.Fatalf("two replicas over two hours pulled %v, want 2 — one per floor", w.f.pulled)
	}
}

// probePass runs the probe pass alone at start+offset.
func (w *credWorld) probePass(offset time.Duration) {
	now := w.start.Add(offset)
	w.f.setNow(now)
	w.s.sweepRetryProbes(context.Background(), w.f, now)
}

// A probe still out is no answer, and no second probe of its window is taken
// while it is — a floor shorter than the marker's TTL makes the difference.
// The canary's pod runs and has not been running long enough to say it got
// past the wall.
func TestCredentialProbe_AProbeStillRunningHoldsTheWindowsNextProbe(t *testing.T) {
	t.Setenv(EnvRetryProbeFloor, "30m")
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("run-a", []string{"fp-zai"}, w.start.Add(-30*time.Minute), wall)
	w.f.park("run-b", []string{"fp-zai"}, w.start.Add(-20*time.Minute), wall)
	w.onResume = func(id string) {
		w.f.twin(id).Mutate(func(run *store.Run) { run.Status = store.RunStatusRunning })
	}
	w.run(0, store.ProbeMarkerTTL-time.Minute)
	if len(w.f.pulled) != 1 {
		t.Fatalf("pulls before the canary's answer = %v, want the canary alone — its probe is still out at the %s floor", w.f.pulled, 30*time.Minute)
	}
}

// The same while the canary's pod has not started: a resume still queued
// within the marker's TTL has not reached the provider, and its probe is out.
func TestCredentialProbe_AProbeStillQueuedHoldsTheWindowsNextProbe(t *testing.T) {
	t.Setenv(EnvRetryProbeFloor, "30m")
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("run-a", []string{"fp-zai"}, w.start.Add(-30*time.Minute), wall)
	w.f.park("run-b", []string{"fp-zai"}, w.start.Add(-20*time.Minute), wall)
	w.run(0, store.ProbeMarkerTTL-time.Minute)
	if len(w.f.pulled) != 1 {
		t.Fatalf("pulls while the canary's resume is queued = %v, want the canary alone", w.f.pulled)
	}
}

// A replica holds a live lease on the earliest-parked wait: the canary is the
// next one, never a row another replica is acting on.
func TestCredentialProbe_ALeasedWaitIsNotTheCanary(t *testing.T) {
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("run-a", []string{"fp-zai"}, w.start.Add(-30*time.Minute), wall)
	w.f.park("run-b", []string{"fp-zai"}, w.start.Add(-20*time.Minute), wall)
	claimed := w.start.Add(-time.Minute)
	w.f.twin("run-a").Mutate(func(run *store.Run) { run.RetryState.ClaimedAt = &claimed })
	w.probePass(0)
	if got := w.f.pullsAt(w.start); !slices.Equal(got, []string{"run-b"}) {
		t.Fatalf("pulls = %v, want run-b — run-a is leased by a replica acting on it", got)
	}
}

// A wait coming due within the floor is the due pass's: pulling it as the
// canary buys nothing, and the window's probe goes to a wait the floor does
// not reach.
func TestCredentialProbe_AWaitDueWithinTheFloorIsNotTheCanary(t *testing.T) {
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("run-a", []string{"fp-zai"}, w.start.Add(-30*time.Minute), wall)
	w.f.park("run-b", []string{"fp-zai"}, w.start.Add(-20*time.Minute), wall)
	soon := w.start.Add(30 * time.Minute)
	w.f.twin("run-a").Mutate(func(run *store.Run) { run.RetryState.RetryAfter = &soon })
	w.probePass(0)
	if got := w.f.pullsAt(w.start); !slices.Equal(got, []string{"run-b"}) {
		t.Fatalf("pulls = %v, want run-b — run-a comes due in 30m, within the floor", got)
	}
}

// Two probes of the window got through, pulled at different instants: the
// answer is the LATEST of them, so a wait parked between the two pulls is
// released on it.
func TestCredentialProbe_TheLatestAnswerReleasesTheWaitsParkedBeforeIt(t *testing.T) {
	t.Setenv(EnvRetryProbeFloor, "2h")
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	announced := wall.Add(time.Minute).Truncate(time.Millisecond)
	probed := func(id string, pulledAt time.Time, status store.RunStatus) {
		w.f.park(id, []string{"fp-zai"}, w.start.Add(-3*time.Hour), wall)
		at := pulledAt.Truncate(time.Millisecond)
		w.f.twin(id).Mutate(func(run *store.Run) {
			run.Status = status
			run.RetryState.RetryAfter = &at
			run.RetryState.ProbeFrom = &announced
			run.RetryState.LastProbeAt = &at
			run.RetryState.ProbeCount = 1
		})
	}
	probed("probe-early", w.start.Add(-90*time.Minute), store.RunStatusRunning)
	probed("probe-late", w.start.Add(-10*time.Minute), store.RunStatusFinished)
	w.f.park("wait-mid", []string{"fp-zai"}, w.start.Add(-30*time.Minute), wall)
	w.probePass(0)
	if got := w.f.pullsAt(w.start); !slices.Equal(got, []string{"wait-mid"}) {
		t.Fatalf("pulls = %v, want wait-mid released — parked 30m ago, before the probe pulled 10m ago that got through", got)
	}
}

// A marker with no pull stamp — a row older than the stamp — says nothing
// about the wall: it releases nobody, and the window is probed as usual.
func TestCredentialProbe_AMarkerWithoutItsPullStampAnswersNothing(t *testing.T) {
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("legacy", []string{"fp-zai"}, w.start.Add(-3*time.Hour), wall)
	announced := wall.Add(time.Minute).Truncate(time.Millisecond)
	pulled := w.start.Add(-5 * time.Minute).Truncate(time.Millisecond)
	w.f.twin("legacy").Mutate(func(run *store.Run) {
		run.RetryState.RetryAfter = &pulled
		run.RetryState.ProbeFrom = &announced
	})
	w.f.park("run-b", []string{"fp-zai"}, w.start.Add(-30*time.Minute), wall)
	w.f.park("run-c", []string{"fp-zai"}, w.start.Add(-20*time.Minute), wall)
	w.probePass(0)
	if got := w.f.pullsAt(w.start); !slices.Equal(got, []string{"run-b"}) {
		t.Fatalf("pulls = %v, want run-b alone as the window's canary — a marker without its pull stamp is no answer to release on", got)
	}
}

// A wait whose window no longer holds it — the credential its launch passed
// over has reopened — is the per-run probe's, not a candidate canary of its
// credential window: the window's canary is a wait the wall still holds, and
// both are asked.
func TestCredentialProbe_AWaitItsWindowNoLongerHoldsIsNotTheCanary(t *testing.T) {
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("run-a", []string{"fp-zai"}, w.start.Add(-30*time.Minute), wall)
	w.f.park("run-b", []string{"fp-zai"}, w.start.Add(-20*time.Minute), wall)
	reopened := w.start.Add(-time.Hour)
	w.f.twin("run-a").Mutate(func(run *store.Run) { run.SkippedCredReopensAt = &reopened })
	w.probePass(0)
	got := w.f.pullsAt(w.start)
	slices.Sort(got)
	if !slices.Equal(got, []string{"run-a", "run-b"}) {
		t.Fatalf("pulls = %v, want run-a probed on its own and run-b as the window's canary", got)
	}
}

// The pass that releases a window on its probe's answer takes no new probe of
// it: the window just got its answer. A wait parked after the probe was
// pulled is not released on it — and is not pulled as a fresh canary in the
// same pass either, though the floor since that pull has run out.
func TestCredentialProbe_APassThatReleasesTakesNoNewProbe(t *testing.T) {
	w := newCredWorld(t)
	wall := w.start.Add(4 * 24 * time.Hour)
	w.f.park("run-a", []string{"fp-zai"}, w.start.Add(-30*time.Minute), wall)
	w.f.park("run-b", []string{"fp-zai"}, w.start.Add(-20*time.Minute), wall)
	// run-b was a canary once, long ago: run-late, never pulled, is the next
	// canary in the window's order — the one a new probe would take.
	longAgo := w.start.Add(-3 * time.Hour).Truncate(time.Millisecond)
	w.f.twin("run-b").Mutate(func(run *store.Run) { run.RetryState.LastProbeAt = &longAgo })
	w.onResume = func(id string) {
		w.f.twin(id).Mutate(func(run *store.Run) { run.Status = store.RunStatusRunning })
	}
	w.run(0, 10*time.Minute)
	w.f.park("run-late", []string{"fp-zai"}, w.start.Add(10*time.Minute), wall)
	w.onResume = nil
	w.run(11*time.Minute, store.ProbeMarkerTTL)
	answer := w.start.Add(store.ProbeMarkerTTL)
	if got := w.f.pullsAt(answer); !slices.Equal(got, []string{"run-b"}) {
		t.Fatalf("pulls in the pass that read the canary's answer = %v, want run-b released alone — run-late met the wall after the pull, and the window just got its answer", got)
	}
}

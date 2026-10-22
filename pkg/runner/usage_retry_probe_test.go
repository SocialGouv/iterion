package runner

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A pull-forward probe (pkg/server/retry_probe.go) resumes a parked run to
// ask ONE question of the provider: did your window reopen early? The answer
// comes from the usage preflight, which refuses before anything spends — so
// the re-failure that follows a probe is not an attempt, and charging it
// would retire the run before the wall it waits on falls.

// What the execution that is parking spent on the provider — the fact
// armUsageWindowRetry reads the probe marker through. A refusal costs nothing
// (the probe's own case); tokens on the clock mean the window reopened, this
// run used it, and the park is a new wall.
const (
	spentNothing = false
	spentTokens  = true
)

// probeResult reads one labelled series of the probe counter.
func probeResult(t *testing.T, reg *metrics.Registry, result string) float64 {
	t.Helper()
	c, err := reg.RunsRetryProbes.GetMetricWithLabelValues(result)
	if err != nil {
		t.Fatalf("probe counter for result %q: %v", result, err)
	}
	return counterValue(t, c)
}

func probedRunDoc(id string, announced time.Time, attempts int) *store.Run {
	probedAt := time.Now().UTC()
	return &store.Run{
		ID:     id,
		Status: store.RunStatusFailedResumable,
		// retrycoord keys the shared circuit on the workflow hash; a fixture
		// without one never reaches the circuit path, and the witnesses that
		// count what a park feeds it count nothing.
		WorkflowHash: "wf-hash-" + id,
		RetryPolicy:  &store.RunRetryPolicy{UsageWindow: "resume", MaxAttempts: 5, MaxWait: "192h", Jitter: "0s"},
		RetryState: &store.RunRetryState{
			RetryAfter: &probedAt,
			ProbeFrom:  &announced,
			// The wall the park that opened this wait wrote: the probe's
			// pull moves retry_after and the marker, never the wall, so a
			// pulled row carries both. A fixture without it models the
			// LEGACY shape (armed before the field existed), where every
			// reader keyed on the wall behaves differently — that shape has
			// its own witnesses.
			Wall:       &announced,
			ProbeCount: 3,
			// The pull's own stamp, written in the same update as the
			// marker: a fixture without it models a row no pull produces,
			// and the payment question (who owes this park) is answered from
			// the age of the pull.
			LastProbeAt: &probedAt,
			Attempts:    attempts,
			Reason:      "usage_window",
			Code:        "USAGE_LIMIT_BLOCKED",
		},
	}
}

func TestArmUsageWindowRetry_AReFailureAfterAProbeSpendsNoAttempt(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	st := &cancelAwareStore{run: probedRunDoc("run-2247", wall, 2)}
	reg := metrics.New()
	r := &Runner{cfg: Config{Store: st, Metrics: reg}}

	got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-2247", spentNothing, iterlog.New(iterlog.LevelError, io.Discard))

	if got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	// This park IS the probe's answer, and the only disposition a pod can
	// report: the preflight refused again, so the pull found the window
	// exactly where the provider said it was. An operator weighing the
	// pass's pods against its hits reads the three results against each
	// other, so a probe that observed something and counted nothing makes
	// every wasted pod look useful.
	if v := probeResult(t, reg, metrics.RetryProbeStillBlocked); v != 1 {
		t.Errorf("iterion_runs_retry_probes_total{result=%s} = %v, want 1", metrics.RetryProbeStillBlocked, v)
	}
	for _, other := range []string{metrics.RetryProbePulled, metrics.RetryProbeStoodDown} {
		if v := probeResult(t, reg, other); v != 0 {
			t.Errorf("iterion_runs_retry_probes_total{result=%s} = %v, want 0 — the runner observes neither the pull nor the stand-down", other, v)
		}
	}
	if !st.probeRearmed {
		t.Fatal("the probed re-failure did not take the free re-arming — hourly probes against a four-day reset spend the whole attempt budget in five hours")
	}
	if st.armed {
		t.Fatalf("ScheduleRunRetry armed at %s and charged an attempt for a failure that followed a probe", st.armedAt)
	}
	if want := wall.Add(time.Minute); !st.probeRearmedAt.Equal(want) {
		t.Errorf("re-armed at %s, want the provider's fresh instant %s", st.probeRearmedAt, want)
	}
	// The run's timeline has to say which arming this was: a free re-arm
	// after a probe and a paid one are the same line otherwise, and the
	// attempt it reports is what an operator counts the budget with.
	if len(st.events) != 1 {
		t.Fatalf("timeline events = %d, want the one run_retry_scheduled", len(st.events))
	}
	if src, _ := st.events[0].Data["reset_source"].(string); !strings.Contains(src, "+after_probe") {
		t.Errorf("reset_source = %q, want it to name the probe the arming followed", src)
	}
	if got, _ := st.events[0].Data["attempt"].(int); got != 2 {
		t.Errorf("event attempt = %d, want the unchanged 2", got)
	}
}

// The sharpest edge: a probe taken when the budget is already spent. The paid
// arming would be REFUSED (the budget is in the store's filter) and the run
// abandoned — the probe would end the run it exists to save.
func TestArmUsageWindowRetry_AProbedReFailureArmsEvenWithTheBudgetSpent(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	st := &cancelAwareStore{run: probedRunDoc("run-2247", wall, 5)} // 5 of 5 spent
	r := &Runner{cfg: Config{Store: st}}

	got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-2247", spentNothing, iterlog.New(iterlog.LevelError, io.Discard))

	if got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed — a probe must not retire a run on an attempt it did not spend", got)
	}
	if !st.probeRearmed {
		t.Fatal("the free re-arming did not run with the budget spent")
	}
}

// The control: no marker, no free ride. Every ordinary usage-window failure
// still spends its attempt.
func TestArmUsageWindowRetry_WithoutAProbeMarkerTheArmingIsPaid(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	run := probedRunDoc("run-plain", wall, 2)
	run.RetryState.ProbeFrom = nil
	st := &cancelAwareStore{run: run}
	r := &Runner{cfg: Config{Store: st}}

	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-plain", spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if st.probeRearmed {
		t.Fatal("an unprobed failure took the free re-arming — the attempt budget would never bind")
	}
	if !st.armed {
		t.Fatal("an unprobed failure armed nothing")
	}
}

// A store blip on the free path must not cost the run its durable retry: say
// so and fall back to the paid arming, which is still a retry.
func TestArmUsageWindowRetry_AFailedFreeReArmFallsBackToThePaidOne(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	st := &cancelAwareStore{run: probedRunDoc("run-2247", wall, 2), probeRearmErr: errors.New("mongo is down")}
	r := &Runner{cfg: Config{Store: st}}

	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-2247", spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed — the fallback is what keeps the run from being dropped", got)
	}
	if !st.armed {
		t.Fatal("a failed free re-arm armed nothing at all: the run is parked with no retry")
	}
}

// The probe's own primary case: a re-failure whose evidence names NO instant.
// Two of usageWindowEvidence's three sources carry none (runtime_code and the
// flattened text, the measured one), and so does a usage-cap preflight whose
// decision has no ResetsAt — the instant usageWindowRetryAt returns is then a
// guess, now+usageWindowBlindWait. Writing that guess over the wall the probe
// set aside deletes the only instant the provider ever named: the row drops
// below the probe floor, every later re-failure is PAID, and five hourly ones
// retire the run days before its wall falls. The announced instant wins.
func TestArmUsageWindowRetry_AProbedBlindReFailureKeepsTheAnnouncedInstant(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	st := &cancelAwareStore{run: probedRunDoc("run-2247", wall, 2)}
	r := &Runner{cfg: Config{Store: st}}

	got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), "run-2247", spentNothing, iterlog.New(iterlog.LevelError, io.Discard))

	if got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if !st.probeRearmed {
		t.Fatal("the blind probed re-failure did not take the free re-arming")
	}
	if !st.probeRearmedAt.Equal(wall) {
		t.Fatalf("re-armed at %s, want the announced instant %s the probe set aside — a %s blind guess written over it deletes the wall, and the paid treadmill that follows retires the run %s before it falls",
			st.probeRearmedAt.Format(time.RFC3339), wall.Format(time.RFC3339), usageWindowBlindWait, time.Until(wall).Round(time.Hour))
	}
	if src, _ := st.events[0].Data["reset_source"].(string); !strings.Contains(src, "+announced_restored") {
		t.Errorf("reset_source = %q, want it to say the announced instant was kept over a blind guess", src)
	}
}

// The other edge of the same rule: good news from the provider is not a
// probe's to override. A DATED re-failure wins even when it names an instant
// EARLIER than the wall the probe set aside.
func TestArmUsageWindowRetry_AProbedDatedReFailureTakesTheFreshInstant(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	fresh := time.Now().UTC().Add(2 * time.Hour)
	st := &cancelAwareStore{run: probedRunDoc("run-2247", wall, 2)}
	r := &Runner{cfg: Config{Store: st}}

	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(fresh), "run-2247", spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if want := fresh.Add(time.Minute); !st.probeRearmedAt.Equal(want) {
		t.Fatalf("re-armed at %s, want the provider's fresh instant %s — restoring the older wall would make the run sleep through a window it was told had moved",
			st.probeRearmedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// The control: an UNPROBED blind refusal still gets the blind wait. The
// restore is the probe's, not a change to what a plain usage window means.
func TestArmUsageWindowRetry_AnUnprobedBlindRefusalStillTakesTheBlindWait(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	run := probedRunDoc("run-plain", wall, 2)
	run.RetryState.ProbeFrom = nil
	st := &cancelAwareStore{run: run}
	r := &Runner{cfg: Config{Store: st}}

	before := time.Now().UTC()
	if got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), "run-plain", spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if !st.armed {
		t.Fatal("an unprobed blind refusal armed nothing")
	}
	if d := st.armedAt.Sub(before); d < usageWindowBlindWait-time.Minute || d > usageWindowBlindWait+time.Minute {
		t.Fatalf("armed in %s, want the %s blind wait: an unprobed row has no announced instant to restore", d, usageWindowBlindWait)
	}
}

// A store blip on the free re-arm falls back to the paid arming — and on a
// run whose budget is already spent (the very case the free path exists for)
// that arming is REFUSED, after which AbandonRunRetry deletes the announced
// instant for good. A settlement is the one answer this path may never give
// on a maybe: hand the delivery back instead, the row keeps its marker, and
// the next pod walks the same path.
func TestArmUsageWindowRetry_AStoreBlipNeverRetiresABudgetSpentProbedRun(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	st := &cancelAwareStore{run: probedRunDoc("run-2247", wall, 5), probeRearmErr: errors.New("mongo is down")}
	r := &Runner{cfg: Config{Store: st}}

	got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-2247", spentNothing, iterlog.New(iterlog.LevelError, io.Discard))

	if len(st.abandoned) != 0 {
		t.Fatalf("a store blip retired a probed run %s before the instant the provider announced: %q", time.Until(wall).Round(time.Hour), st.abandoned)
	}
	if got != usageRetryNotApplicable {
		t.Fatalf("outcome = %v, want usageRetryNotApplicable — a redelivery costs a pod, a settlement costs the run", got)
	}
	if st.run.RetryState.ProbeFrom == nil {
		t.Fatal("the announced instant was consumed anyway: nothing can put it back")
	}
}

// The same blip, transient: the restore lands on the second call, free of
// charge, and the run keeps the wait the provider named.
func TestArmUsageWindowRetry_ATransientBlipRestoresTheAnnouncedInstantForFree(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	st := &cancelAwareStore{
		run:               probedRunDoc("run-2247", wall, 5),
		probeRearmErr:     errors.New("mongo is down"),
		probeRearmErrOnce: true,
	}
	r := &Runner{cfg: Config{Store: st}}

	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-2247", spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if len(st.abandoned) != 0 {
		t.Fatalf("the run was retired anyway: %q", st.abandoned)
	}
	if !st.probeRearmedAt.Equal(wall) {
		t.Fatalf("re-armed at %s, want the announced instant %s", st.probeRearmedAt.Format(time.RFC3339), wall.Format(time.RFC3339))
	}
	if st.armed {
		t.Fatalf("the restore charged an attempt (armed at %s): a probed row's budget is already spent, and spending from it is how the probe ends the run it exists to save", st.armedAt)
	}
}

// A probe's re-failure is not a NEW provider block and not independent
// evidence of a provider-wide storm: the block was counted when the wait was
// armed, and the shared circuit (one breaker per workflow revision, opening
// at a handful of failures) would be fed up to a whole probe budget of
// self-inflicted refusals, delaying the wake of unrelated siblings. What a
// probed re-failure IS is a park, so the scheduled/resumed pair — the one
// signal "armed and never resumed" has — stays comparable.
func TestArmUsageWindowRetry_AProbedReFailureIsNeitherANewBlockNorACircuitFailure(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	run := probedRunDoc("run-2247", wall, 2)
	run.WorkflowHash = "wf-hash-1" // retrycoord keys the shared circuit on it
	st := &cancelAwareStore{run: run}
	reg := metrics.New()
	r := &Runner{cfg: Config{Store: st, Metrics: reg}}

	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-2247", spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if got := counterValue(t, reg.RunsUsageWindowBlocked); got != 0 {
		t.Errorf("iterion_runs_usage_window_blocked_total = %v after a probed re-failure, want 0: one wall is one block, and a 24-probe budget would report 25", got)
	}
	if st.circuitFailures != 0 {
		t.Errorf("the shared retry circuit recorded %d failures for a speculative probe — the breaker opens on a handful and then delays every sibling of the same workflow revision", st.circuitFailures)
	}
	if got := counterValue(t, reg.RunsRetryScheduled); got != 1 {
		t.Errorf("iterion_runs_retry_scheduled_total = %v after a free re-arm, want 1: the sweeper counts the probe's resume in iterion_runs_retry_resumed_total, so a park nobody counts makes resumed outrun scheduled for good", got)
	}
}

// The control: without a marker all three still move, exactly as before.
func TestArmUsageWindowRetry_AnUnprobedReFailureCountsTheBlockAndTheCircuit(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	run := probedRunDoc("run-plain", wall, 2)
	run.RetryState.ProbeFrom = nil
	run.WorkflowHash = "wf-hash-1"
	st := &cancelAwareStore{run: run}
	reg := metrics.New()
	r := &Runner{cfg: Config{Store: st, Metrics: reg}}

	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-plain", spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if got := counterValue(t, reg.RunsUsageWindowBlocked); got != 1 {
		t.Errorf("iterion_runs_usage_window_blocked_total = %v, want 1", got)
	}
	if st.circuitFailures != 1 {
		t.Errorf("shared circuit records = %d, want 1", st.circuitFailures)
	}
	if got := counterValue(t, reg.RunsRetryScheduled); got != 1 {
		t.Errorf("iterion_runs_retry_scheduled_total = %v, want 1", got)
	}
}

// reservesLastAttempt declines the skipped credential's earlier reopening when
// the arming about to happen is the LAST one the budget allows — because every
// arming spends one attempt, and spending the last on a gamble leaves nothing
// for the wall. On a probe-marked row that premise is false: the arming is
// free, so there is nothing to reserve, and declining a free early wake makes
// a probed run sleep through a credential that has already reopened — the very
// symptom the probe exists to cure.
// A row holding the provider's announced instant does not trade it for a
// skipped credential's reopening, on either arming. The trade looks free — the
// wake is sooner and the arming charges nothing — but RearmProbedRunRetry
// consumes probe_from in the same write, so the only durable copy of the
// provider's word is gone: the row comes due hours later, the refusal that
// meets it names no instant, and the hourly treadmill retires the run days
// before its window reopens (measured at the production defaults: five
// one-hour guesses, retired 96h early).
//
// While the wait's ledger is under its budget the cost is the probe FLOOR and
// not the wall: the row re-armed on its wall keeps a ledger under budget, so
// the next probe pulls it forward and gives that credential its chance, free —
// pinned by TheCostOfKeepingTheWallIsOneProbeFloor below. At the budget's last
// probe the cost is the rest of the wait, which
// ASpentProbeBudgetCostsTheSkippedCredentialTheRestOfTheWait measures.
func TestArmUsageWindowRetry_AFreeReArmKeepsTheAnnouncedWallOverASkippedCredential(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	reopens := time.Now().UTC().Add(3 * time.Hour)
	run := probedRunDoc("run-2247", wall, 4) // 4 of 5: the next arming is the last the budget allows
	run.SkippedCredReopensAt = &reopens
	st := &cancelAwareStore{run: run}
	r := &Runner{cfg: Config{Store: st}}

	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-2247", spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if want := wall.Add(time.Minute); !st.probeRearmedAt.Equal(want) {
		t.Fatalf("re-armed at %s, want the provider's announced instant %s: the arming that takes the early wake consumes the marker in the same write, and the wall is then held nowhere",
			st.probeRearmedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if st.armed {
		t.Errorf("ScheduleRunRetry charged an attempt at %s: a probe's re-failure is still free", st.armedAt.Format(time.RFC3339))
	}
	if src, _ := st.events[0].Data["reset_source"].(string); strings.Contains(src, "skipped_credential") {
		t.Errorf("reset_source = %q, want no skipped credential: the decision never weighed it on a row holding the wall", src)
	}
	// The run's timeline still REPORTS the reopening — it is a fact about the
	// run either way, and an operator reading the event must see what the
	// arming knew.
	if got, _ := st.events[0].Data["skipped_cred_reopens_at"].(string); got == "" {
		t.Errorf("the timeline event dropped the skipped credential's reopening: %v", st.events[0].Data)
	}
}

// The PAID arming is the same site twice: a probe pulled the row forward, the
// resume got past the provider and spent tokens, and the park that follows is
// charged — but it is charged on the wall the row is holding, not on a skipped
// credential's reopening. The spend decides who pays and only that; the
// instant is the row's, and ScheduleRunRetry deletes the marker in the same
// write, so a traded wall is traded for good.
func TestArmUsageWindowRetry_APaidParkOnAProbedRowKeepsTheAnnouncedWall(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	reopens := time.Now().UTC().Add(90 * time.Minute)
	run := probedRunDoc("run-2247", wall, 1)
	run.SkippedCredReopensAt = &reopens
	run.WorkflowHash = "wf-hash-1"
	st := &cancelAwareStore{run: run}
	r := &Runner{cfg: Config{Store: st, Metrics: metrics.New()}}

	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-2247", spentTokens,
		iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if st.probeRearmed {
		t.Errorf("a park that spent tokens was re-armed free of charge at %s", st.probeRearmedAt.Format(time.RFC3339))
	}
	if want := wall.Add(time.Minute); !st.armed || !st.armedAt.Equal(want) {
		t.Fatalf("armed(paid)=%v at %s, want the announced instant %s: the paid arming consumes the marker, so a wall traded here is held nowhere",
			st.armed, st.armedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// What keeping the wall costs WHILE THE LEDGER IS UNDER ITS BUDGET, measured
// against the scan that bounds it: one probe floor, not the days between the
// reopening and the wall. The row re-armed on its wall carries a ledger under
// budget and no marker, which is exactly what the probe scan lists — so the
// credential that already reopened gets its chance at the next pull, free of
// charge. The other side of the bound — the budget's last probe, where the
// cost becomes the rest of the wait — is
// TestArmUsageWindowRetry_ASpentProbeBudgetCostsTheSkippedCredentialTheRestOfTheWait.
func TestArmUsageWindowRetry_TheCostOfKeepingTheWallIsOneProbeFloor(t *testing.T) {
	ctx := context.Background()
	quiet := iterlog.New(iterlog.LevelError, io.Discard)
	w := newProbeWorld()
	now := time.Now().UTC()
	wall := now.Add(96 * time.Hour)
	reopens := now.Add(3 * time.Hour)

	if out := w.r.armUsageWindowRetry(ctx, weeklyWindowErr(wall), w.runID, spentNothing, quiet); out != usageRetryArmed {
		t.Fatalf("the park that opens the wall: %v", out)
	}
	armed := w.st.Row().RetryAfter
	if armed == nil {
		t.Fatal("nothing armed after the park")
	}
	if pulled, err := w.st.PullRunRetry(ctx, w.runID, *armed, now); err != nil || !pulled {
		t.Fatalf("pull = %v, %v — the probe pass could not take the row", pulled, err)
	}
	if err := w.st.ClearRunRetry(ctx, w.runID); err != nil {
		t.Fatalf("the resume's clear: %v", err)
	}
	w.st.Mutate(func(run *store.Run) { run.SkippedCredReopensAt = &reopens })
	if out := w.r.armUsageWindowRetry(ctx, weeklyWindowErr(wall), w.runID, spentNothing, quiet); out != usageRetryArmed {
		t.Fatalf("the probed re-failure: %v", out)
	}

	row := w.st.Row()
	if row.RetryAfter == nil || !row.RetryAfter.Equal(wall.Add(time.Minute).Truncate(time.Millisecond)) {
		t.Fatalf("the free re-arm left the row on %v, want the announced wall %s", row.RetryAfter, wall.Add(time.Minute).Format(time.RFC3339))
	}
	if rows := w.st.ProbeRows(now.Add(time.Minute), probePropertyFloor, probePropertyProbeBudget, 2); len(rows) != 0 {
		t.Errorf("the probe scan listed the row one minute after the pull: the pace must hold it for one floor")
	}
	rows := w.st.ProbeRows(now.Add(probePropertyFloor+time.Minute), probePropertyFloor, probePropertyProbeBudget, 2)
	if len(rows) != 1 {
		t.Fatalf("the probe scan listed %d row(s) one floor later, want 1: a row re-armed on its wall must be pullable again, or keeping the wall really would cost the days the credential reopened early",
			len(rows))
	}
}

// probeTheWait spends n probes of one armed wait through the REAL scan: one
// pull a floor, each followed by the resume's clear and the probed
// re-failure's free re-arm, which puts the row back on `wall` with the ledger
// exactly where the pull left it. It leaves the row armed and unmarked, and
// returns the instant of the last pull. A pull the scan would have refused is
// a state no pass produces, so it fails the test rather than inventing one.
func probeTheWait(t *testing.T, w *probeWorld, wall, from time.Time, n int) time.Time {
	t.Helper()
	ctx := context.Background()
	quiet := iterlog.New(iterlog.LevelError, io.Discard)
	var last time.Time
	for i := 1; i <= n; i++ {
		last = from.Add(time.Duration(i) * probePropertyFloor)
		rows := w.st.ProbeRows(last, probePropertyFloor, probePropertyProbeBudget, 2)
		if len(rows) != 1 || rows[0].RetryAfter == nil {
			t.Fatalf("probe %d of %d: the scan listed %d row(s) at probe_count=%d/%d, want 1",
				i, n, len(rows), w.st.Row().ProbeCount, probePropertyProbeBudget)
		}
		if pulled, err := w.st.PullRunRetry(ctx, w.runID, *rows[0].RetryAfter, last); err != nil || !pulled {
			t.Fatalf("probe %d of %d: pull = %v, %v", i, n, pulled, err)
		}
		if err := w.st.ClearRunRetry(ctx, w.runID); err != nil {
			t.Fatalf("probe %d of %d: the resume's clear: %v", i, n, err)
		}
		if out := w.r.armUsageWindowRetry(ctx, weeklyWindowErr(wall), w.runID, spentNothing, quiet); out != usageRetryArmed {
			t.Fatalf("probe %d of %d: the probed re-failure: %v", i, n, out)
		}
	}
	return last
}

// The budget's LAST probe: the pull that takes the ledger to its bound is
// legal (the scan admits probe_count < budget) and the free re-arm that
// follows leaves it there, so no pass lists that wait again. The arming
// declines the skipped credential's reopening like every arming of a row
// holding the announced instant — and here that costs the credential the rest
// of the wait, not one floor. The cost of keeping the provider's word, stated
// in store.ProbeMarkerTTL's neighbourhood and in docs/scheduling.md, and
// measured here: a run that sleeps to its wall while a key it could have used
// has already reopened.
func TestArmUsageWindowRetry_ASpentProbeBudgetCostsTheSkippedCredentialTheRestOfTheWait(t *testing.T) {
	ctx := context.Background()
	quiet := iterlog.New(iterlog.LevelError, io.Discard)
	w := newProbeWorld()
	now := time.Now().UTC()
	wall := now.Add(96 * time.Hour)
	reopens := now.Add(3 * time.Hour)

	if out := w.r.armUsageWindowRetry(ctx, weeklyWindowErr(wall), w.runID, spentNothing, quiet); out != usageRetryArmed {
		t.Fatalf("the park that opens the wait: %v", out)
	}
	// Every probe but the last, each one leaving the row on its wall.
	last := probeTheWait(t, w, wall, now, probePropertyProbeBudget-1)

	// The budget's last pull, and the resume that follows it re-resolves the
	// credential chain — this time passing over a key that reopens in three
	// hours.
	rows := w.st.ProbeRows(last.Add(probePropertyFloor), probePropertyFloor, probePropertyProbeBudget, 2)
	if len(rows) != 1 {
		t.Fatalf("the budget's last probe: the scan listed %d row(s) at probe_count=%d/%d, want 1 — probe_count < budget admits it",
			len(rows), w.st.Row().ProbeCount, probePropertyProbeBudget)
	}
	if pulled, err := w.st.PullRunRetry(ctx, w.runID, *rows[0].RetryAfter, last.Add(probePropertyFloor)); err != nil || !pulled {
		t.Fatalf("the budget's last pull = %v, %v", pulled, err)
	}
	if err := w.st.ClearRunRetry(ctx, w.runID); err != nil {
		t.Fatalf("the resume's clear: %v", err)
	}
	w.st.Mutate(func(run *store.Run) { run.SkippedCredReopensAt = &reopens })
	if out := w.r.armUsageWindowRetry(ctx, weeklyWindowErr(wall), w.runID, spentNothing, quiet); out != usageRetryArmed {
		t.Fatalf("the probed re-failure on the budget's last probe: %v", out)
	}

	row := w.st.Row()
	if row.ProbeCount != probePropertyProbeBudget {
		t.Fatalf("probe_count = %d after %d pulls, want the budget %d: a free re-arm that reset the ledger would bound nothing",
			row.ProbeCount, probePropertyProbeBudget, probePropertyProbeBudget)
	}
	if w.st.FreeRearms != probePropertyProbeBudget || row.Attempts != 1 {
		t.Fatalf("free re-arms = %d, attempts = %d, want %d and the opening park's 1: every re-arm here is the probe's own",
			w.st.FreeRearms, row.Attempts, probePropertyProbeBudget)
	}
	if want := wall.Add(time.Minute).Truncate(time.Millisecond); row.RetryAfter == nil || !row.RetryAfter.Equal(want) {
		t.Fatalf("the last free re-arm left the row on %v, want the announced wall %s: the earlier wake would consume the marker in the same write and leave the wall held nowhere",
			row.RetryAfter, want.Format(time.RFC3339))
	}
	// And nothing lists the wait again, at any later instant — so the three-hour
	// reopening gets no further chance and the run sleeps to the wall.
	for _, at := range []time.Time{now.Add(probePropertyFloor * time.Duration(probePropertyProbeBudget+1)), now.Add(25 * time.Hour), now.Add(72 * time.Hour)} {
		if got := w.st.ProbeRows(at, probePropertyFloor, probePropertyProbeBudget, 2); len(got) != 0 {
			t.Fatalf("the probe scan listed the row again at %s with probe_count=%d/%d: the per-wait budget is the bound",
				at.Format(time.RFC3339), w.st.Row().ProbeCount, probePropertyProbeBudget)
		}
	}
	if slept := row.RetryAfter.Sub(reopens); slept < 90*time.Hour {
		t.Fatalf("the run sleeps %s past the reopening, want the rest of the wait (~93h): the measured cost of keeping the wall at the budget's edge", slept)
	}
}

// The marker's payment answer expires with the PULL, so a park whose pull is
// older than store.ProbeMarkerTTL is charged an attempt — the mild error that
// constant names. And that is ALL it costs: the instant it arms is the SAME
// announced one restoreAnnouncedInstant just put back, so it brings no news
// about the wall, keeps that wall's probe ledger and its pace, and buys the
// window no second budget. The charged parks of one wall are bounded by
// max_attempts; its probes stay bounded by the budget.
func TestArmUsageWindowRetry_AStaleMarkerParkIsChargedAndKeepsTheWallsProbeLedger(t *testing.T) {
	ctx := context.Background()
	quiet := iterlog.New(iterlog.LevelError, io.Discard)
	w := newProbeWorld()
	now := time.Now().UTC()
	wall := now.Add(96 * time.Hour)

	if out := w.r.armUsageWindowRetry(ctx, weeklyWindowErr(wall), w.runID, spentNothing, quiet); out != usageRetryArmed {
		t.Fatalf("the park that opens the wait: %v", out)
	}
	// Spend the budget but the last probe, then take the last pull and leave
	// the marker LIVE: this resume is the one the shared circuit holds.
	last := probeTheWait(t, w, wall, now, probePropertyProbeBudget-1)
	rows := w.st.ProbeRows(last.Add(probePropertyFloor), probePropertyFloor, probePropertyProbeBudget, 2)
	if len(rows) != 1 {
		t.Fatalf("the budget's last probe: the scan listed %d row(s), want 1", len(rows))
	}
	if pulled, err := w.st.PullRunRetry(ctx, w.runID, *rows[0].RetryAfter, last.Add(probePropertyFloor)); err != nil || !pulled {
		t.Fatalf("the budget's last pull = %v, %v", pulled, err)
	}
	if err := w.st.ClearRunRetry(ctx, w.runID); err != nil {
		t.Fatalf("the resume's clear: %v", err)
	}
	// The one fact no test can drive: every writer of the stamp uses the real
	// clock, so the AGE of the pull is set rather than waited for. This is the
	// row a resume the circuit deferred past the TTL leaves behind.
	stale := time.Now().UTC().Add(-2 * store.ProbeMarkerTTL)
	w.st.Mutate(func(run *store.Run) { run.RetryState.LastProbeAt = &stale })
	before := w.st.Row()
	if before.ProbeCount != probePropertyProbeBudget || before.ProbeFrom == nil {
		t.Fatalf("before the park: probe_count=%d probe_from=%v, want the budget %d and a live marker",
			before.ProbeCount, before.ProbeFrom, probePropertyProbeBudget)
	}

	if out := w.r.armUsageWindowRetry(ctx, blindUsageWindowErr(), w.runID, spentNothing, quiet); out != usageRetryArmed {
		t.Fatalf("the stale-marker park: %v", out)
	}

	row := w.st.Row()
	if w.st.PaidArmings != 2 || row.Attempts != 2 {
		t.Fatalf("paid armings = %d, attempts = %d, want 2 and 2: a park whose pull is older than %s is nobody's probe and pays its own way",
			w.st.PaidArmings, row.Attempts, store.ProbeMarkerTTL)
	}
	if want := before.ProbeFrom.Truncate(time.Millisecond); row.RetryAfter == nil || !row.RetryAfter.Equal(want) {
		t.Fatalf("armed for %v, want the announced instant %s put back: the WALL does not expire with the pull",
			row.RetryAfter, want.Format(time.RFC3339))
	}
	// The ledger belongs to the WALL, and this park re-stated the wall the row
	// was already on: the budget and the pace survive it.
	if row.ProbeCount != probePropertyProbeBudget || row.LastProbeAt == nil {
		t.Fatalf("after the charged park: probe_count=%d last_probe_at=%v, want the wall's own ledger kept at %d with its pace — a charged park that re-states the wall must buy the window no second budget",
			row.ProbeCount, row.LastProbeAt, probePropertyProbeBudget)
	}
	// And so the ceiling on this wall is the budget, reached here: no pass
	// lists it again, and the run sleeps to the instant the provider named.
	if got := w.st.ProbeRows(time.Now().UTC().Add(time.Minute), probePropertyFloor, probePropertyProbeBudget, 2); len(got) != 0 {
		t.Fatalf("the probe scan listed %d row(s) after the charged park, want 0: this wall has spent its %d probes, and a wait handed a fresh budget per charged park costs max_attempts × budget pods",
			len(got), probePropertyProbeBudget)
	}
}

// Another paid arming that re-arms the SAME wall: the store refuses the free
// re-arm, and the fall-through restores the announced instant and keeps that
// wall's ledger exactly like the stale-marker park above. What makes an arming
// paid differs; what a restoring one does to the wall does not — the rule over
// every shape is derived in
// TestArmUsageWindowRetry_ThePaidArmingsThatRestoreTheAnnouncedInstant.
func TestArmUsageWindowRetry_ARefusedFreeReArmAlsoKeepsTheWallsProbeLedger(t *testing.T) {
	ctx := context.Background()
	quiet := iterlog.New(iterlog.LevelError, io.Discard)
	w := newProbeWorld()
	now := time.Now().UTC()
	wall := now.Add(96 * time.Hour)

	if out := w.r.armUsageWindowRetry(ctx, weeklyWindowErr(wall), w.runID, spentNothing, quiet); out != usageRetryArmed {
		t.Fatalf("the park that opens the wait: %v", out)
	}
	armed := w.st.Row().RetryAfter
	if armed == nil {
		t.Fatal("nothing armed after the park")
	}
	if pulled, err := w.st.PullRunRetry(ctx, w.runID, *armed, now); err != nil || !pulled {
		t.Fatalf("pull = %v, %v — the probe pass could not take the row", pulled, err)
	}
	if err := w.st.ClearRunRetry(ctx, w.runID); err != nil {
		t.Fatalf("the resume's clear: %v", err)
	}
	w.st.RearmErr = errors.New("mongo is down")

	if out := w.r.armUsageWindowRetry(ctx, blindUsageWindowErr(), w.runID, spentNothing, quiet); out != usageRetryArmed {
		t.Fatalf("the probed re-failure whose free re-arm the store refused: %v", out)
	}

	row := w.st.Row()
	if w.st.FreeRearms != 0 || w.st.PaidArmings != 2 || row.Attempts != 2 {
		t.Fatalf("free re-arms = %d, paid armings = %d, attempts = %d, want 0, 2 and 2: a store blip hands the retry back for a price",
			w.st.FreeRearms, w.st.PaidArmings, row.Attempts)
	}
	if want := armed.Truncate(time.Millisecond); row.RetryAfter == nil || !row.RetryAfter.Equal(want) {
		t.Fatalf("armed for %v, want the announced instant %s rather than the one-hour guess the blind refusal named",
			row.RetryAfter, want.Format(time.RFC3339))
	}
	if row.ProbeCount != 1 || row.LastProbeAt == nil {
		t.Fatalf("after the fall-through: probe_count=%d last_probe_at=%v, want the wall's ledger kept at 1 with its pace — the same wall, said again",
			row.ProbeCount, row.LastProbeAt)
	}
}

// The control at the paid edge: WITHOUT a marker the reservation still binds,
// so the last attempt goes to the wall and not to the gamble.
func TestArmUsageWindowRetry_AnUnprobedLastAttemptStillPinsTheWall(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	reopens := time.Now().UTC().Add(3 * time.Hour)
	run := probedRunDoc("run-plain", wall, 4)
	run.RetryState.ProbeFrom = nil
	run.SkippedCredReopensAt = &reopens
	st := &cancelAwareStore{run: run}
	r := &Runner{cfg: Config{Store: st}}

	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-plain", spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if want := wall.Add(time.Minute); !st.armedAt.Equal(want) {
		t.Fatalf("armed at %s, want the authoritative wall %s: the last PAID attempt must not be spent on another credential's reopening",
			st.armedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// The paid FALL-THROUGH arms the same instant for a price, so the announced
// one has to survive there too: a store blip on the free re-arm must not let
// a one-hour guess be written over the provider's wall and charged an attempt
// on top.
func TestArmUsageWindowRetry_ThePaidFallbackAlsoKeepsTheAnnouncedInstant(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	st := &cancelAwareStore{run: probedRunDoc("run-2247", wall, 2), probeRearmErr: errors.New("mongo is down")}
	r := &Runner{cfg: Config{Store: st}}

	if got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), "run-2247", spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if !st.armed {
		t.Fatal("the fallback armed nothing at all")
	}
	if !st.armedAt.Equal(wall) {
		t.Fatalf("the paid fallback armed %s, want the announced instant %s: a %s guess charged an attempt AND deleted the wall",
			st.armedAt.Format(time.RFC3339), wall.Format(time.RFC3339), usageWindowBlindWait)
	}
}

// The control on the other side of the restore: when the marker is GONE — the
// sweeper stood the probe down, or an operator resumed the run — this is no
// longer a probed row, and today's settlement is the right answer. A guard
// that deferred here would leave a genuinely exhausted run armed forever.
func TestArmUsageWindowRetry_WithTheMarkerGoneAnExhaustedRunStillSettles(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	st := &cancelAwareStore{run: probedRunDoc("run-2247", wall, 5), probeRearmRefuse: true}
	r := &Runner{cfg: Config{Store: st}}

	got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-2247", spentNothing, iterlog.New(iterlog.LevelError, io.Discard))

	if got != usageRetryExhausted {
		t.Fatalf("outcome = %v, want usageRetryExhausted", got)
	}
	if len(st.abandoned) != 1 {
		t.Fatalf("settlements = %v, want the one that says the budget is spent", st.abandoned)
	}
}

// The reservation is PATH-scoped, and this is why: on the paid fall-through
// the premise holds again (that arming does spend an attempt), so the last one
// the budget allows must still go to the wall and not to another credential's
// reopening. A spend-blind decision — zeroing the attempt count for the whole
// call because the row carries a marker — would spend it on the gamble and
// retire the run before the wall falls.
func TestArmUsageWindowRetry_ThePaidFallbackStillPinsTheWallOnItsLastAttempt(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	reopens := time.Now().UTC().Add(3 * time.Hour)
	run := probedRunDoc("run-2247", wall, 4) // 4 of 5: the fallback arming is the last
	run.SkippedCredReopensAt = &reopens
	st := &cancelAwareStore{run: run, probeRearmErr: errors.New("mongo is down")}
	r := &Runner{cfg: Config{Store: st}}

	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(wall), "run-2247", spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if want := wall.Add(time.Minute); !st.armedAt.Equal(want) {
		t.Fatalf("the paid fallback armed %s, want the authoritative wall %s: it charged the last attempt the budget allows to a speculative wake, so nothing is left for the wall this run is actually waiting on",
			st.armedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// The wall is kept even when another credential reopens sooner. The rule
// "a re-failure that names no instant of its own keeps the announced one" is
// read from the REFUSAL, not from the reset source: the skipped-credential
// branch replaces that string wholesale, and a reader keying on it silently
// stopped firing on exactly these rows — the free re-arm then consumed the
// marker, the row landed inside the probe floor where no probe can reach it,
// and every later blind re-failure was paid until the budget retired the run
// days before its wall fell.
func TestArmUsageWindowRetry_ABlindReFailureKeepsTheWallThroughTheSkippedCredential(t *testing.T) {
	now := time.Now().UTC()
	wall := now.Add(4 * 24 * time.Hour)
	reopens := now.Add(30 * time.Minute)
	run := probedRunDoc("run-2247", wall, 2)
	run.SkippedCredReopensAt = &reopens
	st := &cancelAwareStore{run: run}
	r := &Runner{cfg: Config{Store: st}}

	if got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), "run-2247", spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if !st.probeRearmedAt.Equal(wall) {
		t.Fatalf("re-armed at %s, want the announced wall %s: the refusal named no instant, so the %s guess (or a credential's reopening taken over it) is all the row keeps, and the wall %s away is gone with the consumed marker",
			st.probeRearmedAt.Format(time.RFC3339), wall.Format(time.RFC3339), usageWindowBlindWait, time.Until(wall).Round(time.Hour))
	}
	if src, _ := st.events[0].Data["reset_source"].(string); !strings.Contains(src, announcedRestoredSource) {
		t.Errorf("reset_source = %q, want it to name the restore", src)
	}
}

// A park that followed REAL WORK is not the probe's answer. The marker
// deliberately outlives the resume it was written for (the pod reads it), so
// a run the probe pulled forward, that resumed because the window really had
// reopened and burnt it again, still carries it. Tokens on the clock are what
// tells the two apart: this park is an ordinary new wall, charged, counted,
// fed to the shared circuit, and left with the ledger cleared so the pull
// request hears about it.
func TestArmUsageWindowRetry_AParkThatFollowedRealWorkIsCharged(t *testing.T) {
	now := time.Now().UTC()
	deadWall := now.Add(48 * time.Hour) // announced for the window the run has since used up
	fresh := now.Add(90 * time.Minute)  // and what the provider says now
	run := probedRunDoc("run-worked", deadWall, 2)
	run.WorkflowHash = "wf-hash-1"
	st := &cancelAwareStore{run: run}
	reg := metrics.New()
	r := &Runner{cfg: Config{Store: st, Metrics: reg}}

	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(fresh), "run-worked", spentTokens, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if st.probeRearmed {
		t.Fatalf("the park was re-armed free of charge at %s: an execution that spent tokens got PAST the provider, so its park is a new wall and not the probe's refusal",
			st.probeRearmedAt.Format(time.RFC3339))
	}
	if !st.armed {
		t.Fatal("nothing was armed")
	}
	if want := fresh.Add(time.Minute); !st.armedAt.Equal(want) {
		t.Errorf("armed at %s, want the provider's fresh instant %s: the instant held aside belongs to a window this run has already reopened and closed again",
			st.armedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got := counterValue(t, reg.RunsUsageWindowBlocked); got != 1 {
		t.Errorf("iterion_runs_usage_window_blocked_total = %v, want 1: this is a wall nothing has counted yet", got)
	}
	if st.circuitFailures != 1 {
		t.Errorf("shared circuit records = %d, want 1: a run that burnt a reopened window is evidence for the cohort", st.circuitFailures)
	}
	if run.RetryState.ProbeCount != 0 {
		t.Errorf("probe_count = %d after a paid arming, want 0: the pull request's pause notice stands down while it is set, so a genuinely new wall would never be announced",
			run.RetryState.ProbeCount)
	}
}

// Two places restore the probe's held-aside instant — the decision and the
// paid arming that is refused and falls back to it — and the run's timeline
// must not read as two restores.
func TestArmUsageWindowRetry_TheRestoredInstantIsTaggedOnce(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	st := &cancelAwareStore{
		run:               probedRunDoc("run-2247", wall, 5), // budget spent: the paid arming is refused
		probeRearmErr:     errors.New("mongo is down"),
		probeRearmErrOnce: true,
	}
	r := &Runner{cfg: Config{Store: st}}

	if got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), "run-2247", spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	src, _ := st.events[0].Data["reset_source"].(string)
	if n := strings.Count(src, announcedRestoredSource); n != 1 {
		t.Errorf("reset_source = %q names the restore %d times, want 1", src, n)
	}
}

// The predicate itself, through the producers that feed it. A park's verdict
// rests on it: read too narrowly (a price, say) and a forfait-served run —
// tokens on the clock, no price anywhere — parks as if the provider had
// never answered, which hands a probe's free re-arm to a wall nobody held
// aside.
func TestSpentOnTheProvider_ReadsWhatTheExecutionWrote(t *testing.T) {
	ctx := context.Background()

	if (*metricsEmitter)(nil).spentOnTheProvider() {
		t.Error("a nil emitter (the refusal came before any executor was built) reports spend")
	}

	refused := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	if refused.spentOnTheProvider() {
		t.Error("an execution that wrote no usage event reports spend — that is the probe's own case, and charging it retires the run early")
	}

	// A claw step the rate table cannot price: tokens, cost 0 — the shape a
	// forfait-backed run writes.
	step := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	if _, err := step.AppendEvent(ctx, "run-step", store.Event{
		Type: store.EventLLMStepFinished, RunID: "run-step", NodeID: "n1",
		Data: map[string]any{"input_tokens": float64(1200), "output_tokens": float64(300)},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if cost, in, _, _ := step.RunTotals(); cost != 0 || in == 0 {
		t.Fatalf("RunTotals cost=%v input=%d, want an unpriced step with tokens — the case this asserts", cost, in)
	}
	if !step.spentOnTheProvider() {
		t.Error("1500 tokens and no price: the provider answered, so the park that follows is a new wall and not a probe's refusal")
	}

	// A delegation, through the hook the backends really call.
	deleg := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	hooks := model.NewStoreEventHooks(ctx, deleg, "run-deleg", iterlog.New(iterlog.LevelError, io.Discard), nil, nil)
	hooks.OnDelegateFinished("n1", model.DelegateInfo{BackendName: "claude_code", Tokens: 900})
	if cost, _, _, agg := deleg.RunTotals(); cost != 0 || agg != 900 {
		t.Fatalf("RunTotals cost=%v aggregate=%d, want 900 unpriced aggregate tokens", cost, agg)
	}
	if !deleg.spentOnTheProvider() {
		t.Error("a delegation reported 900 tokens and the predicate says the provider never answered")
	}
}

// The wiring, from the caller the runner really uses: parkUsageLimitRetry
// reads the execution's spend and the arming follows it. Asserted here
// because the decision half takes the verdict as a bool — a park that read
// it from nowhere would keep every test above green.
func TestParkUsageLimitRetry_TheExecutionsSpendDecidesWhoPays(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	msg := &queue.RunMessage{RunID: "run-2247"}
	logger := iterlog.New(iterlog.LevelError, io.Discard)

	t.Run("a refusal that spent nothing re-arms free", func(t *testing.T) {
		st := &cancelAwareStore{run: probedRunDoc("run-2247", wall, 2)}
		r := &Runner{cfg: Config{Store: st}}
		d := &fakeDelivery{}

		handled, status := r.parkUsageLimitRetry(context.Background(), weeklyWindowErr(wall), d, msg,
			newMetricsEmitter(&recordingEmitter{}, metrics.New()), logger)

		if !handled || status != "usage_limit_retry" || d.acks != 1 {
			t.Fatalf("handled=%v status=%q acks=%d, want an acked usage-limit park", handled, status, d.acks)
		}
		if !st.probeRearmed {
			t.Error("the park was charged: a probe's refusal spends no attempt")
		}
	})

	t.Run("a park that spent tokens is charged", func(t *testing.T) {
		st := &cancelAwareStore{run: probedRunDoc("run-2247", wall, 2)}
		r := &Runner{cfg: Config{Store: st}}
		d := &fakeDelivery{}
		usage := newMetricsEmitter(&recordingEmitter{}, metrics.New())
		hooks := model.NewStoreEventHooks(context.Background(), usage, "run-2247", logger, nil, nil)
		hooks.OnDelegateFinished("n1", model.DelegateInfo{BackendName: "claude_code", Tokens: 4096})

		handled, status := r.parkUsageLimitRetry(context.Background(), weeklyWindowErr(wall), d, msg, usage, logger)

		if !handled || status != "usage_limit_retry" || d.acks != 1 {
			t.Fatalf("handled=%v status=%q acks=%d, want an acked usage-limit park", handled, status, d.acks)
		}
		if st.probeRearmed {
			t.Error("an execution that spent 4096 tokens got past the provider, so its park is a new wall — it must be charged, counted and announced")
		}
		if !st.armed {
			t.Error("nothing was armed")
		}
	})
}

// usageWindowProbedRetryAt cannot refuse, and that is load-bearing: it
// returns an instant and a source, with no "no decision" to fall back from.
// What makes it total is that usageWindowRetryAt refuses on the error and
// the policy alone — never on the attempt count, which the probed path pins
// at zero. A refusal keyed on attempts would arm the probed path on a zero
// instant: unreachable from here, which is why it is pinned here.
func TestUsageWindowRetryAt_NeverRefusesOnTheAttemptCount(t *testing.T) {
	now := time.Now().UTC()
	pol := noJitter(retrypolicy.Policy{MaxAttempts: 5})
	err := weeklyWindowErr(now.Add(4 * 24 * time.Hour))

	for _, spent := range []int{0, 1, 4, 5, 6, 99} {
		at, _, ok := usageWindowRetryAt(err, pol, now, time.Time{}, spent)
		if !ok || at.IsZero() {
			t.Fatalf("attemptsSpent=%d: ok=%v at=%s — the probed decision has no refusal to handle, so a refusal here arms nothing",
				spent, ok, at.Format(time.RFC3339))
		}
	}
}

// The spend decides WHO PAYS for the arming. It says nothing about WHEN the
// window reopens, and the two facts had been read from one marker: a park
// that reported spend lost the wall the probe had set aside and took a
// one-hour guess in its place.
//
// The spend here is written by the real producer, in the shape that makes the
// premise false: a node whose every route was REFUSED, one of them after
// burning tokens on a session that died. model.dispatchChain folds a failed
// chain's spend into the error's Result ("An exhausted chain still spent what
// its routes burned") and the delegate_error hook carries it to the run's
// totals — so "tokens on the clock" is not proof that anything got past the
// wall.
func TestArmUsageWindowRetry_ABlindReFailureKeepsTheWallWhoeverPays(t *testing.T) {
	now := time.Now().UTC()
	wall := now.Add(4 * 24 * time.Hour)
	logger := iterlog.New(iterlog.LevelError, io.Discard)

	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	hooks := model.NewStoreEventHooks(context.Background(), usage, "run-2247", logger, nil, nil)
	hooks.OnDelegateError("n1", model.DelegateInfo{
		BackendName: "claude_code",
		Tokens:      1200,
		Error:       blindUsageWindowErr(),
	})
	if !usage.spentOnTheProvider() {
		t.Fatal("precondition: a chain that burned tokens on a route that died reports spend, refused or not")
	}

	run := probedRunDoc("run-2247", wall, 2)
	run.WorkflowHash = "wf-hash-1"
	st := &cancelAwareStore{run: run}
	reg := metrics.New()
	r := &Runner{cfg: Config{Store: st, Metrics: reg}}

	if got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), "run-2247", usage.spentOnTheProvider(), logger); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if !st.armedAt.Equal(wall) {
		t.Errorf("armed at %s, want the announced wall %s: a %s guess wrote over the only dated fact this row has, and hourly guesses spend the attempt budget in five hours",
			st.armedAt.Format(time.RFC3339), wall.Format(time.RFC3339), usageWindowBlindWait)
	}
	// The payment half is untouched: this park is still the caller's to pay
	// for, to count and to announce.
	if !st.armed || st.probeRearmed {
		t.Errorf("armed(paid)=%v re-armed(free)=%v, want the PAID arming: the execution's spend is what decides who pays", st.armed, st.probeRearmed)
	}
	if got := counterValue(t, reg.RunsUsageWindowBlocked); got != 1 {
		t.Errorf("iterion_runs_usage_window_blocked_total = %v, want 1", got)
	}
	if st.circuitFailures != 1 {
		t.Errorf("shared circuit records = %d, want 1", st.circuitFailures)
	}
	// The ledger belongs to the WALL, and this park re-stated the wall the row
	// was already on: it keeps the budget and the pace, so the pull request
	// hears nothing new and the next probe of this window is still this
	// window's.
	if run.RetryState.ProbeCount != 3 {
		t.Errorf("probe_count = %d after a charged park that restored the same wall, want the wall's own ledger kept at 3: a budget that restarts here prices one window at max_attempts × budget",
			run.RetryState.ProbeCount)
	}
	if st.armedWall != (time.Time{}) {
		t.Errorf("the arming carried wall %s, want the zero time: a park whose refusal named no instant of its own brings no news about where the wall is",
			st.armedWall.Format(time.RFC3339))
	}
}

// Retiring a run is the shortest life there is, so the attempt budget must
// not end one whose announced wall is still ahead — whatever the execution
// spent. The paid arming is refused exactly when the budget is gone, and
// AbandonRunRetry deletes the announced instant with it.
func TestArmUsageWindowRetry_ASpentParkOnAnExhaustedBudgetKeepsTheWall(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	run := probedRunDoc("run-spent", wall, 5) // budget spent: ScheduleRunRetry refuses
	st := &cancelAwareStore{run: run}
	r := &Runner{cfg: Config{Store: st}}

	got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), "run-spent", spentTokens, iterlog.New(iterlog.LevelError, io.Discard))

	if got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed — the run was retired %s before the instant its provider named", got, time.Until(wall).Round(time.Hour))
	}
	if len(st.abandoned) != 0 {
		t.Fatalf("settlements = %v, want none", st.abandoned)
	}
	if !st.probeRearmed || !st.probeRearmedAt.Equal(wall) {
		t.Errorf("re-armed=%v at %s, want the announced wall %s restored free of charge (the charge was REFUSED, so nothing is spent either way)",
			st.probeRearmed, st.probeRearmedAt.Format(time.RFC3339), wall.Format(time.RFC3339))
	}
}

// The loop as production runs it: the probe pulls the row forward, the
// preflight refuses, the run parks, and round it goes once per probe floor.
// Every one of these parks reports spend — the folded tokens of a chain whose
// routes all failed — so every one is charged, and the budget is five. The
// run must still be waiting on its wall at the end of it: a speculative pull
// may give a run an earlier chance to resume, never a shorter life.
func TestArmUsageWindowRetry_TheProbeLoopNeverRetiresARunWhoseWallIsAhead(t *testing.T) {
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	run := probedRunDoc("run-loop", wall, 0)
	st := &cancelAwareStore{run: run}
	r := &Runner{cfg: Config{Store: st}}
	logger := iterlog.New(iterlog.LevelError, io.Discard)

	// One more cycle than the budget allows, so the refused arming is
	// exercised rather than assumed.
	for cycle := 0; cycle <= 5; cycle++ {
		// What store/mongo.PullRunRetry writes: the announced instant goes
		// aside, retry_after comes to now. (The fake's ScheduleRunRetry
		// clears the marker exactly as the real one does, so each cycle
		// re-establishes it.)
		armed := wall
		run.RetryState.ProbeFrom = &armed
		before := run.RetryState.Attempts

		got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), "run-loop", spentTokens, logger)
		if got != usageRetryArmed {
			t.Fatalf("cycle %d (attempt %d/5): outcome = %v, want usageRetryArmed — settlements %v",
				cycle, before, got, st.abandoned)
		}
		if at := armedInstant(st); !at.Equal(wall) {
			t.Fatalf("cycle %d: the row is armed at %s, want the announced wall %s", cycle, at.Format(time.RFC3339), wall.Format(time.RFC3339))
		}
		if st.armed {
			// The real ScheduleRunRetry's CAS increments attempts; the fake
			// reads the document, so the cycle carries the increment.
			run.RetryState.Attempts = before + 1
			st.armed = false
		}
	}
	if len(st.abandoned) != 0 {
		t.Fatalf("settlements = %v, want none: the run waits on the instant its provider named", st.abandoned)
	}
	if run.RetryState.Attempts != 5 {
		t.Errorf("attempts spent = %d, want the budget's 5 and not one more", run.RetryState.Attempts)
	}
}

// armedInstant is where the row ended up, whichever arming landed it there.
func armedInstant(st *cancelAwareStore) time.Time {
	if st.armed {
		return st.armedAt
	}
	return st.probeRearmedAt
}

// The marker outlives the resume it was written for on purpose — the pod that
// parks has to read it — and nothing ever deletes it but an arming. A
// probe-driven resume that never parks (it succeeded, its pod was evicted)
// therefore leaves it on the row for good, and a park days later would be
// taken for that long-finished probe's own refusal: free, uncounted, unfed to
// the circuit and silent on the pull request, which stands down while the
// probe ledger is set.
// The other shape of a marker nobody is paying for, and the one the instant
// cannot catch: the probe RESUMED and that execution is long gone (it
// succeeded, its pod was evicted), while the instant it set aside is still
// days ahead. Only an arming deletes the marker and ClearRunRetry keeps it on
// purpose, so the row carries it for the rest of the wait — six more days on a
// weekly reset probed on day one — and the next park, which is nobody's probe,
// would take the whole free treatment: no attempt, no block counted, no
// evidence for the cohort, and a probe ledger left open, which is what the
// pause notice stands down on.
//
// Who pays expires with the PULL. The wall does not: the row still holds the
// provider's own dated word, and the paid arming below both charges its
// attempt and keeps it.
func TestArmUsageWindowRetry_AMarkerFromAProbeThatNeverParkedPaysItsOwnWay(t *testing.T) {
	now := time.Now().UTC()
	announced := now.Add(7 * 24 * time.Hour) // a weekly reset, probed 36h ago
	run := probedRunDoc("run-stale-pull", announced, 2)
	run.WorkflowHash = "wf-hash-1"
	pulledLongAgo := now.Add(-36 * time.Hour)
	run.RetryState.LastProbeAt = &pulledLongAgo
	st := &cancelAwareStore{run: run}
	reg := metrics.New()
	r := &Runner{cfg: Config{Store: st, Metrics: reg}}

	// A genuinely NEW wall, dated, 36 hours out.
	fresh := now.Add(36 * time.Hour)
	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(fresh), "run-stale-pull", spentNothing,
		iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if st.probeRearmed {
		t.Fatalf("the park was re-armed FREE of charge at %s on a marker whose probe resumed %s ago",
			st.probeRearmedAt.Format(time.RFC3339), now.Sub(pulledLongAgo).Round(time.Hour))
	}
	if want := fresh.Add(time.Minute); !st.armed || !st.armedAt.Equal(want) {
		t.Errorf("armed(paid)=%v at %s, want the paid arming on the fresh instant %s: this park is nobody's probe",
			st.armed, st.armedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got := counterValue(t, reg.RunsUsageWindowBlocked); got != 1 {
		t.Errorf("iterion_runs_usage_window_blocked_total = %v, want 1: a provider block nothing has counted", got)
	}
	if st.circuitFailures != 1 {
		t.Errorf("shared circuit records = %d, want 1: a wall nobody held aside is evidence for the cohort", st.circuitFailures)
	}
	if run.RetryState.ProbeCount != 0 {
		t.Errorf("probe_count = %d, want 0: the pause notice stands down while it is set, so the pull request would never hear about this wall",
			run.RetryState.ProbeCount)
	}
}

// And the wall it keeps, on the same row: a refusal that names NO instant is a
// one-hour guess, and the announced instant beats it however old the pull is.
// Taking the guess instead is the disease, not the cure — a row armed inside
// the probe floor is not probe-eligible, so the next park is another guess,
// and five of them retire the run days before its window reopens.
//
// The row below is armed on the wall with that wall's ledger intact, which is
// the bound the budget states: this one has spent its probes, so no pass lists
// it again and the run sleeps to the announced instant. What the paid arming
// buys is the WALL, never a second budget on it.
func TestArmUsageWindowRetry_AStaleProbePullKeepsTheAnnouncedWallOverAGuess(t *testing.T) {
	ctx := context.Background()
	quiet := iterlog.New(iterlog.LevelError, io.Discard)
	now := time.Now().UTC()
	announced := now.Add(7 * 24 * time.Hour)

	w := newProbeWorld()
	pulledLongAgo := now.Add(-36 * time.Hour).Truncate(time.Millisecond)
	probeInstant := now.Truncate(time.Millisecond)
	w.st.Mutate(func(run *store.Run) {
		run.RetryState = &store.RunRetryState{
			ProbeFrom: &announced, ProbeCount: 3, LastProbeAt: &pulledLongAgo,
			RetryAfter: &probeInstant, Attempts: 1,
			Reason: "usage_window", Code: "USAGE_LIMIT_BLOCKED",
		}
	})
	if got := w.r.armUsageWindowRetry(ctx, blindUsageWindowErr(), w.runID, spentNothing, quiet); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	row := w.st.Row()
	if row.RetryAfter == nil || row.RetryAfter.Before(announced.Truncate(time.Millisecond)) {
		t.Fatalf("armed at %v, want the announced instant %s or later: a guess written over the provider's own word deletes the only wall this run was given",
			row.RetryAfter, announced.Format(time.RFC3339))
	}
	if row.Attempts != 2 {
		t.Errorf("attempts = %d, want 2: the park is nobody's probe and pays for itself", row.Attempts)
	}
	if row.ProbeFrom != nil {
		t.Errorf("the arming left the probe MARKER on the row (%v): the next park would take the free treatment on a wall nobody is probing", row.ProbeFrom)
	}
	if row.ProbeCount != 3 || row.LastProbeAt == nil {
		t.Errorf("the paid arming reopened the wall's ledger: probe_count=%d last_probe_at=%v, want it kept at 3 with its pace",
			row.ProbeCount, row.LastProbeAt)
	}
	// And so the bound is the budget, stated: this wall has spent its probes,
	// so no pass lists the row again and the run waits out the instant the
	// provider named.
	if rows := w.st.ProbeRows(now.Add(time.Minute), probePropertyFloor, probePropertyProbeBudget, 2); len(rows) != 0 {
		t.Fatalf("the probe scan listed %d row(s), want 0: a wall whose %d probes are spent must stop being probed, or a charged park would hand it a fresh budget every attempt",
			len(rows), probePropertyProbeBudget)
	}
}

func TestArmUsageWindowRetry_AMarkerFromAFinishedWaitIsNotAProbe(t *testing.T) {
	now := time.Now().UTC()
	dead := now.Add(-4 * 24 * time.Hour) // the instant that wait was held for, long past
	fresh := now.Add(72 * time.Hour)     // the genuinely new wall this park carries
	run := probedRunDoc("run-stale", dead, 2)
	run.WorkflowHash = "wf-hash-1"
	st := &cancelAwareStore{run: run}
	reg := metrics.New()
	r := &Runner{cfg: Config{Store: st, Metrics: reg}}

	if got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(fresh), "run-stale", spentNothing, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if st.probeRearmed {
		t.Fatalf("the park was re-armed free of charge at %s on a marker whose wait ended %s ago",
			st.probeRearmedAt.Format(time.RFC3339), now.Sub(dead).Round(time.Hour))
	}
	if want := fresh.Add(time.Minute); !st.armed || !st.armedAt.Equal(want) {
		t.Errorf("armed(paid)=%v at %s, want the paid arming on the fresh instant %s", st.armed, st.armedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got := counterValue(t, reg.RunsUsageWindowBlocked); got != 1 {
		t.Errorf("iterion_runs_usage_window_blocked_total = %v, want 1: this is a provider block nothing has counted yet", got)
	}
	if st.circuitFailures != 1 {
		t.Errorf("shared circuit records = %d, want 1: a wall nobody held aside is evidence for the cohort", st.circuitFailures)
	}
	if run.RetryState.ProbeCount != 0 {
		t.Errorf("probe_count = %d, want 0: the pause notice stands down while it is set, so the pull request would never hear about this wall", run.RetryState.ProbeCount)
	}
}

// Which armings re-arm the SAME wait — derived, never listed.
//
// A paid arming normally opens a NEW wall: one attempt, the probe ledger
// closed, the pull request told. One that RESTORES the instant the probe set
// aside does all of that on the wait the row was ALREADY on — the ledger
// reopens under the same instant, so that wait is probed afresh (which is why
// one wait costs up to max_attempts × budget pods) and the pause notice names
// the instant the pull request already heard. Three facts decide it, and each
// has its own rule: the age of the PULL and the execution's SPEND say who
// pays, the REFUSAL says which instant. Plus a store that may refuse the free
// re-arm and hand the park back for a price.
//
// The whole cross product drives the real arming here, so what is asserted is
// the RULE on every shape that reaches it rather than a list of the shapes:
// a paid arming that restored the announced instant charges an attempt and
// reopens the wait's ledger. Enumerating those shapes in prose went short
// three times — one, then two, then three — because the three facts compose;
// derived, a fourth shape needs no new sentence anywhere.
func TestArmUsageWindowRetry_ThePaidArmingsThatRestoreTheAnnouncedInstant(t *testing.T) {
	const (
		pullNone  = "no marker"
		pullFresh = "a live pull"
		pullStale = "a pull older than the marker TTL"
	)
	cases := []struct {
		name         string
		pull         string
		spend        bool // the execution got an answer the provider charged for
		blind        bool // the refusal named no instant of its own
		rearmRefused bool // the store refused the free re-arm
		wantPaid     bool
		wantRestored bool
	}{
		{name: "no marker, no spend, a refusal naming no instant", pull: pullNone, blind: true, wantPaid: true},
		{name: "no marker, no spend, a dated refusal", pull: pullNone, wantPaid: true},
		{name: "no marker, real work, a refusal naming no instant", pull: pullNone, spend: true, blind: true, wantPaid: true},
		{name: "no marker, real work, a dated refusal", pull: pullNone, spend: true, wantPaid: true},

		{name: "a live pull, no spend, a refusal naming no instant", pull: pullFresh, blind: true, wantRestored: true},
		{name: "a live pull, no spend, a dated refusal", pull: pullFresh},
		{name: "a live pull, real work, a refusal naming no instant", pull: pullFresh, spend: true, blind: true, wantPaid: true, wantRestored: true},
		{name: "a live pull, real work, a dated refusal", pull: pullFresh, spend: true, wantPaid: true},

		{name: "a stale pull, no spend, a refusal naming no instant", pull: pullStale, blind: true, wantPaid: true, wantRestored: true},
		{name: "a stale pull, no spend, a dated refusal", pull: pullStale, wantPaid: true},
		{name: "a stale pull, real work, a refusal naming no instant", pull: pullStale, spend: true, blind: true, wantPaid: true, wantRestored: true},
		{name: "a stale pull, real work, a dated refusal", pull: pullStale, spend: true, wantPaid: true},

		{name: "a live pull, no spend, a refusal naming no instant, the free re-arm refused", pull: pullFresh, blind: true, rearmRefused: true, wantPaid: true, wantRestored: true},
		{name: "a live pull, no spend, a dated refusal, the free re-arm refused", pull: pullFresh, rearmRefused: true, wantPaid: true},
	}

	// The three ways a park on a probed row is PAID must each reach the
	// restore: one of them missing from the table is how an enumeration of
	// these shapes goes short.
	var withSpend, withStalePull, withRefusedRearm bool
	for _, tc := range cases {
		if !tc.wantPaid || !tc.wantRestored {
			continue
		}
		withSpend = withSpend || tc.spend
		withStalePull = withStalePull || tc.pull == pullStale
		withRefusedRearm = withRefusedRearm || tc.rearmRefused
	}
	if !withSpend || !withStalePull || !withRefusedRearm {
		t.Fatalf("the table drives the restore from spend=%v, a stale pull=%v, a refused free re-arm=%v — all three are paid armings and all three restore, so a table missing one asserts the rule on a slice of it",
			withSpend, withStalePull, withRefusedRearm)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			quiet := iterlog.New(iterlog.LevelError, io.Discard)
			w := newProbeWorld()
			now := time.Now().UTC()
			wall := now.Add(96 * time.Hour)
			dated := now.Add(72 * time.Hour) // what a DATED re-failure names here

			if out := w.r.armUsageWindowRetry(ctx, weeklyWindowErr(wall), w.runID, spentNothing, quiet); out != usageRetryArmed {
				t.Fatalf("the park that opens the wait: %v", out)
			}
			announced := *w.st.Row().RetryAfter
			if tc.pull != pullNone {
				if pulled, err := w.st.PullRunRetry(ctx, w.runID, announced, now); err != nil || !pulled {
					t.Fatalf("the probe's pull = %v, %v", pulled, err)
				}
			}
			// The resume, whoever caused it: ClearRunRetry keeps the marker
			// so the pod that parks can read it.
			if err := w.st.ClearRunRetry(ctx, w.runID); err != nil {
				t.Fatalf("the resume's clear: %v", err)
			}
			if tc.pull == pullStale {
				// Every writer of the stamp uses the real clock, so the AGE
				// of a pull is set rather than waited for.
				stale := time.Now().UTC().Add(-2 * store.ProbeMarkerTTL)
				w.st.Mutate(func(run *store.Run) { run.RetryState.LastProbeAt = &stale })
			}
			if tc.rearmRefused {
				w.st.RearmErr = errors.New("mongo is down")
			}
			before := w.st.Row()

			refusal := weeklyWindowErr(dated)
			if tc.blind {
				refusal = blindUsageWindowErr()
			}
			if out := w.r.armUsageWindowRetry(ctx, refusal, w.runID, tc.spend, quiet); out != usageRetryArmed {
				t.Fatalf("the second park: %v", out)
			}

			row := w.st.Row()
			paid := w.st.PaidArmings == 2
			restored := row.RetryAfter != nil && row.RetryAfter.Equal(announced.Truncate(time.Millisecond))
			if paid != tc.wantPaid || restored != tc.wantRestored {
				t.Fatalf("paid=%v restored=%v, want paid=%v restored=%v — armed for %v, announced %s (paid armings=%d free re-arms=%d)",
					paid, restored, tc.wantPaid, tc.wantRestored, row.RetryAfter, announced.Format(time.RFC3339), w.st.PaidArmings, w.st.FreeRearms)
			}
			if !paid || !restored {
				return
			}
			// The rule, on every shape that reaches it.
			if row.Attempts != before.Attempts+1 {
				t.Errorf("attempts %d -> %d, want exactly one more: every one of these shapes is a CHARGED park, and max_attempts is what bounds them",
					before.Attempts, row.Attempts)
			}
			if row.ProbeFrom != nil {
				t.Errorf("after the restoring park the marker survives (%v): the next park would take the free treatment on a wall nobody is probing", row.ProbeFrom)
			}
			if row.ProbeCount != before.ProbeCount || (before.LastProbeAt != nil) != (row.LastProbeAt != nil) {
				t.Errorf("after the restoring park: probe_count %d -> %d, pace set %v -> %v, want the wall's ledger kept — a park that re-states the instant the pull request already heard buys that window no second budget and says nothing new on it",
					before.ProbeCount, row.ProbeCount, before.LastProbeAt != nil, row.LastProbeAt != nil)
			}
		})
	}
}

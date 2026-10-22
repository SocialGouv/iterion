package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// The probe is the half of the retry that acts on a wait NOBODY has observed:
// a forfait an operator cleared by hand reopens with no notice, and the due
// pass can only ever see `retry_after <= now`. What these tests pin is the
// one guarantee that makes a speculative pull safe — it may give a run an
// earlier chance to resume, never a shorter life and never an attempt.

// probedRef is a due row that a probe pulled forward: retry_after is the
// probe's instant, probe_from keeps the one the provider announced.
func probedRef(id string, probedAt, announced time.Time) mongostore.RetryDueRef {
	ref := dueRef(id, probedAt)
	ref.RetryState.ProbeFrom = &announced
	ref.RetryState.ProbeCount = 1
	// The pull's own stamp, which PullRunRetry writes in the same update as
	// the marker: who pays for a park is answered from the AGE of the pull,
	// so a ref without it models a row no pull produces.
	ref.RetryState.LastProbeAt = &probedAt
	return ref
}

func probedRun(id string, probedAt, announced time.Time) *store.Run {
	return &store.Run{
		ID:     id,
		Status: store.RunStatusFailedResumable,
		RetryState: &store.RunRetryState{
			RetryAfter:  &probedAt,
			ProbeFrom:   &announced,
			ProbeCount:  1,
			LastProbeAt: &probedAt,
			Attempts:    1,
			Reason:      "usage_window",
			Code:        "USAGE_LIMIT_BLOCKED",
		},
	}
}

// A retry armed four days out is invisible to every due pass until that
// instant; the probe pass is what pulls it forward to now so the runner's
// preflight — the only authoritative reading of the provider's window — gets
// to answer today.
func TestSweepRetryProbes_PullsANotYetDueRetryForwardToNow(t *testing.T) {
	st := newFakeRetryStore()
	s := newRetrySweeperServer(t, st, nil)
	s.cfg.Metrics = metrics.New()

	now := time.Now().UTC()
	announced := now.Add(4 * 24 * time.Hour)
	lister := &fakeRetryLister{probeRefs: []mongostore.RetryDueRef{dueRef("run-parked", announced)}}

	s.sweepRetryProbes(context.Background(), lister, now)

	if len(st.pulls) != 1 {
		t.Fatalf("PullRunRetry calls = %d, want 1 — a run armed for %s is never listed by the due pass, so nothing else can observe a manual reset",
			len(st.pulls), announced.Format(time.RFC3339))
	}
	got := st.pulls[0]
	if got.runID != "run-parked" || !got.from.Equal(announced) || !got.to.Equal(now) {
		t.Fatalf("pull = %+v, want run-parked from the announced %s to now %s", got, announced, now)
	}
	if v := probeResultValue(t, s.cfg.Metrics, metrics.RetryProbePulled); v != 1 {
		t.Errorf("iterion_runs_retry_probes_total{result=pulled} = %v, want 1 — the pod a probe may waste has to be countable", v)
	}
}

// The pass carries the OPERATOR's bounds to the scan, and `0` turns probing
// off entirely: the manual sweep stays the escape hatch, so a deployment that
// wants no speculative pod must be able to say so.
func TestSweepRetryProbes_OperatorBoundsReachTheScan(t *testing.T) {
	st := newFakeRetryStore()
	s := newRetrySweeperServer(t, st, nil)
	now := time.Now().UTC()

	t.Setenv(EnvRetryProbeFloor, "15m")
	t.Setenv(EnvRetryProbeBudget, "3")
	lister := &fakeRetryLister{}
	s.sweepRetryProbes(context.Background(), lister, now)
	if len(lister.probeCalls) != 1 {
		t.Fatalf("probe scans = %d, want 1", len(lister.probeCalls))
	}
	if got := lister.probeCalls[0]; got.floor != 15*time.Minute || got.budget != 3 || got.limit != retryProbeBatch {
		t.Fatalf("scan bounds = %+v, want floor 15m, budget 3, limit %d", got, retryProbeBatch)
	}

	t.Setenv(EnvRetryProbeBudget, "0")
	off := &fakeRetryLister{probeRefs: []mongostore.RetryDueRef{dueRef("run-parked", now.Add(48*time.Hour))}}
	s.sweepRetryProbes(context.Background(), off, now)
	if len(off.probeCalls) != 0 {
		t.Errorf("probe scans with budget 0 = %d, want 0 — the pass must be disarmable", len(off.probeCalls))
	}
	if len(st.pulls) != 0 {
		t.Errorf("pulls with budget 0 = %v, want none", st.pulls)
	}
}

// A lost CAS is an ordinary outcome of running on every replica, not a
// failure: the row is not counted as probed and nothing else happens.
func TestSweepRetryProbes_ALostPullIsNotCountedAsAProbe(t *testing.T) {
	st := newFakeRetryStore()
	st.pullOK = false
	s := newRetrySweeperServer(t, st, nil)
	s.cfg.Metrics = metrics.New()

	now := time.Now().UTC()
	lister := &fakeRetryLister{probeRefs: []mongostore.RetryDueRef{dueRef("run-raced", now.Add(72*time.Hour))}}
	s.sweepRetryProbes(context.Background(), lister, now)

	if v := probeResultValue(t, s.cfg.Metrics, metrics.RetryProbePulled); v != 0 {
		t.Errorf("iterion_runs_retry_probes_total{result=pulled} = %v after a lost CAS, want 0", v)
	}
}

// A scan error is reported, never swallowed into "nothing to probe", and it
// must not touch any row. The fake answers with rows AND the error — a
// half-decoded cursor's shape — so dropping the error check pulls a row the
// scan could not vouch for, instead of silently pulling nothing.
func TestSweepRetryProbes_AScanErrorPullsNothing(t *testing.T) {
	st := newFakeRetryStore()
	s := newRetrySweeperServer(t, st, nil)
	var logs bytes.Buffer
	s.logger = iterlog.New(iterlog.LevelWarn, &logs)
	lister := &fakeRetryLister{
		probeRefs: []mongostore.RetryDueRef{dueRef("run-parked", time.Now().UTC().Add(48*time.Hour))},
		probeErr:  errors.New("mongo is down"),
	}

	s.sweepRetryProbes(context.Background(), lister, time.Now().UTC())

	if len(st.pulls) != 0 {
		t.Errorf("pulls after a failed scan = %v, want none", st.pulls)
	}
	if got := logs.String(); !strings.Contains(got, "mongo is down") {
		t.Errorf("the scan error was not reported, so an operator sees an idle probe pass instead of a broken one; logs:\n%s", got)
	}
}

// The probe pass manufactures rows FOR the due pass, so its batch must leave
// that pass room. Both scans sort ASC on retry_after and a row pulled a
// minute ago sorts ahead of a retry that becomes due this second, so a probe
// batch as large as retrySweepBatch fills the entire capacity of the pass
// that resumes genuinely due runs — and pins iterion_runs_retry_pending at
// its cap, which that gauge's own contract reads as "retries are arriving
// faster than they are being resumed".
func TestSweepRetryProbes_TheProbeBatchLeavesTheDuePassRoom(t *testing.T) {
	st := newFakeRetryStore()
	s := newRetrySweeperServer(t, st, nil)
	now := time.Now().UTC()
	var refs []mongostore.RetryDueRef
	for i := 0; i < retrySweepBatch+3; i++ {
		refs = append(refs, dueRef(fmt.Sprintf("run-parked-%d", i), now.Add(48*time.Hour)))
	}
	lister := &fakeRetryLister{probeRefs: refs}

	s.sweepRetryProbes(context.Background(), lister, now)

	if len(lister.probeCalls) != 1 {
		t.Fatalf("probe scans = %d, want 1", len(lister.probeCalls))
	}
	if got := lister.probeCalls[0].limit; got >= retrySweepBatch {
		t.Fatalf("the probe pass asked for %d rows a pass while the due pass can only take %d: every slot a probe takes delays a retry the provider's own instant made due",
			got, retrySweepBatch)
	}
	if len(st.pulls) >= retrySweepBatch {
		t.Fatalf("pulls in one pass = %d, want fewer than the due pass's %d", len(st.pulls), retrySweepBatch)
	}
}

// newMeteredRetrySweeperServer is newRetrySweeperServer with the admission
// gate WIRED: an org and a team for the tenant dueRef names, plus the counter
// that backs the monthly run quota. The ungated twin is kinder than
// production — gateLaunch returns (nil, nil) with no auth store, so it cannot
// see what a resume costs.
func newMeteredRetrySweeperServer(t *testing.T, st store.RunStore) (*Server, *orgusage.MemoryCounter) {
	t.Helper()
	s := newOrgTestServer(t)
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
	seedGate(t, s, gateSpec{id: "team-1"})
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bots", "feed-watch"), 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bots", "feed-watch", "main.bot"), []byte("workflow w:\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	s.cfg.Store, s.cfg.WorkDir, s.cfg.Metrics = st, dir, metrics.New()
	return s, counter
}

func meteredRetryRuns(t *testing.T, c *orgusage.MemoryCounter) int {
	t.Helper()
	u, err := c.Usage(context.Background(), orgusage.OrgSubject("team-1"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return u.Runs
}

// A probe's resume is speculative by construction: the preflight it exists to
// ask refuses before anything spends. It must not be BILLABLE. resumeDueRetry
// admits every resume through the metered gate (gateLaunch's last step is also
// the metering increment) and hands the unit back only on its failure
// branches — and a probe's resume succeeds, the publish lands, the refusal
// happens inside the pod where no admission handle exists. Unrefunded, one
// parked wait spends its whole probe budget of the org's monthly run quota on
// launches whose refusal is expected, and the tenant's REAL launches are
// denied 402 for the rest of the month.
func TestSweepDueRetries_AProbeDrivenResumeIsNotBillable(t *testing.T) {
	st := newFakeRetryStore()
	s, counter := newMeteredRetrySweeperServer(t, st)
	resumer := &fakeResumer{}

	const cycles = 3
	for i := 0; i < cycles; i++ {
		now := time.Now().UTC()
		announced := now.Add(4 * 24 * time.Hour)
		st.claimWins["run-probed"] = true
		s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{probedRef("run-probed", now, announced)}}, resumer, now)
	}

	if len(resumer.calls) != cycles {
		t.Fatalf("Resume called %d times, want %d — the probes did not reach the resume at all", len(resumer.calls), cycles)
	}
	if got := meteredRetryRuns(t, counter); got != 0 {
		t.Fatalf("%d probe-driven resumes metered %d monthly run-quota units, want 0: the default budget is %d probes per wait, so one parked run would spend %d of the org's quota and 402 its real launches",
			cycles, got, defaultRetryProbeBudget, defaultRetryProbeBudget)
	}
	// What the metric table promises an operator: the probe's resumes ARE
	// resumes, so they land in `enqueued` and stay countable against
	// iterion_runs_retry_probes_total.
	enqueued, err := s.cfg.Metrics.RunsRetryResumed.GetMetricWithLabelValues("enqueued")
	if err != nil {
		t.Fatal(err)
	}
	if got := counterValue(t, enqueued); got != cycles {
		t.Errorf("iterion_runs_retry_resumed_total{result=enqueued} = %v, want %d", got, cycles)
	}
}

// And what BOUNDS that refund. The probe's own refusals cost the provider
// nothing and are free as often as the budget allows; the resume that finds
// the window genuinely reopened is the one unmetered execution per wait,
// because the park that follows its real work is PAID (the runner reads the
// execution's own spend — pkg/runner.armUsageWindowRetry) and a paid arming
// clears the probe ledger (store/mongo.ScheduleRunRetry). A marker that
// outlived that park would make the refund fire on every later resume too,
// and the under-count would be the whole probe budget instead of one run.
func TestSweepDueRetries_TheProbeRefundStopsAtThePaidPark(t *testing.T) {
	st := newFakeRetryStore()
	s, counter := newMeteredRetrySweeperServer(t, st)
	resumer := &fakeResumer{}

	for i := 0; i < 3; i++ {
		now := time.Now().UTC()
		st.claimWins["run-probed"] = true
		s.sweepDueRetries(context.Background(),
			&fakeRetryLister{refs: []mongostore.RetryDueRef{probedRef("run-probed", now, now.Add(4*24*time.Hour))}}, resumer, now)
	}
	if got := meteredRetryRuns(t, counter); got != 0 {
		t.Fatalf("probe-driven resumes metered %d units, want 0", got)
	}

	// The window had reopened, the run worked, and its park was charged —
	// so the row the sweeper next reads carries no probe marker.
	now := time.Now().UTC()
	st.claimWins["run-probed"] = true
	s.sweepDueRetries(context.Background(),
		&fakeRetryLister{refs: []mongostore.RetryDueRef{dueRef("run-probed", now.Add(-time.Minute))}}, resumer, now)

	if got := meteredRetryRuns(t, counter); got != 1 {
		t.Fatalf("the resume after the paid park metered %d units, want 1: once the ledger is closed a resume is an ordinary billable launch", got)
	}
}

// The launch-RATE token is the gate's other reservation, and rollback is what
// hands it back: with a bucket of one a minute, an unrefunded probe eats the
// minute's only token and the next probe is denied — so the probe would
// crowd out the pass that resumes runs whose own instant has arrived.
func TestSweepDueRetries_AProbeDrivenResumeRefundsItsLaunchRateToken(t *testing.T) {
	st := newFakeRetryStore()
	s := newOrgTestServer(t)
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
	seedGate(t, s, gateSpec{id: "team-1", launchRatePerMin: 1})
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bots", "feed-watch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bots", "feed-watch", "main.bot"), []byte("workflow w:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.Store, s.cfg.WorkDir = st, dir
	resumer := &fakeResumer{}

	for i := 0; i < 2; i++ {
		now := time.Now().UTC()
		st.claimWins["run-probed"] = true
		s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{probedRef("run-probed", now, now.Add(4*24*time.Hour))}}, resumer, now)
	}

	if len(resumer.calls) != 2 {
		t.Fatalf("resumes = %d, want 2 — the first probe kept the minute's only launch-rate token", len(resumer.calls))
	}
}

// The control: an ordinary due resume — the provider's own instant arrived,
// the run is expected to do real work — is metered exactly as before.
func TestSweepDueRetries_AnOrdinaryResumeIsStillMetered(t *testing.T) {
	st := newFakeRetryStore()
	s, counter := newMeteredRetrySweeperServer(t, st)
	st.claimWins["run-due"] = true
	resumer := &fakeResumer{}
	now := time.Now().UTC()

	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{dueRef("run-due", now.Add(-time.Minute))}}, resumer, now)

	if len(resumer.calls) != 1 {
		t.Fatalf("Resume called %d times, want 1", len(resumer.calls))
	}
	if got := meteredRetryRuns(t, counter); got != 1 {
		t.Fatalf("monthly runs metered = %d after an ordinary resume, want 1: a retry resume meters an execution attempt by design", got)
	}
}

// THE hazard of pulling a run forward: the sweeper's own re-arm charges an
// attempt AND overwrites retry_after with now+15m. On a probed row that
// destroys the instant the provider announced and re-probes every quarter
// hour, spending the whole attempt budget within the hour — the probe would
// retire the run it exists to save. The probe must stand down instead.
func TestSweepDueRetries_AProbedRowSurvivesAFailedResumeWithItsAttemptsAndItsInstant(t *testing.T) {
	st := newFakeRetryStore()
	st.claimWins["run-probed"] = true
	now := time.Now().UTC()
	announced := now.Add(4 * 24 * time.Hour)
	st.loadRun["run-probed"] = probedRun("run-probed", now, announced)
	resumer := &fakeResumer{failing: errors.New("publish blipped")}
	s := newRetrySweeperServer(t, st, resumer)

	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{probedRef("run-probed", now, announced)}}, resumer, now)

	if got, ok := st.probeRearmed["run-probed"]; !ok || !got.Equal(announced) {
		t.Fatalf("probe stand-down re-armed at %v (present=%t), want the announced instant %s", got, ok, announced)
	}
	if at, ok := st.rearmed["run-probed"]; ok {
		t.Errorf("the probed row was re-armed at %s through the paid path — the announced instant %s is lost and the next probe is 15 minutes away", at, announced)
	}
	if budget, ok := st.armBudget["run-probed"]; ok {
		t.Errorf("ScheduleRunRetry charged an attempt (budget seen: %d) for a failure that followed a probe", budget)
	}
	if len(st.abandoned) != 0 {
		t.Errorf("unexpected abandon: %v", st.abandoned)
	}
}

// The other settlement path: a PERMANENT refusal (here a runner-pool remap,
// which the un-probed twin test abandons on) would kill the run days before
// the wall it waits on falls — a decision the announced instant will make
// anyway, and the probe has no business making early.
func TestSweepDueRetries_AProbedRowIsNotRetiredDaysBeforeItsAnnouncedInstant(t *testing.T) {
	st := newFakeRetryStore()
	st.claimWins["run-probed"] = true
	now := time.Now().UTC()
	announced := now.Add(4 * 24 * time.Hour)
	st.loadRun["run-probed"] = probedRun("run-probed", now, announced)
	resumer := &fakeResumer{failing: fmt.Errorf("%w: run %s is bound to runner pool %q but its team is now mapped to %q — resume refused",
		cloudpublisher.ErrPoolRemapped, "run-probed", "old-pool", "new-pool")}
	s := newRetrySweeperServer(t, st, resumer)

	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{probedRef("run-probed", now, announced)}}, resumer, now)

	if reason, ok := st.abandoned["run-probed"]; ok {
		t.Fatalf("a probe-pulled row was abandoned %s before its announced instant (%q)", announced.Sub(now), reason)
	}
	if got, ok := st.probeRearmed["run-probed"]; !ok || !got.Equal(announced) {
		t.Fatalf("probe stand-down re-armed at %v (present=%t), want the announced instant %s", got, ok, announced)
	}
}

// A row with NO marker keeps today's behaviour exactly: the guard must not
// swallow the ordinary abandon.
func TestSweepDueRetries_AnUnprobedRowStillAbandonsOnAPermanentRefusal(t *testing.T) {
	st := newFakeRetryStore()
	st.claimWins["run-plain"] = true
	now := time.Now().UTC()
	resumer := &fakeResumer{failing: fmt.Errorf("%w: pool moved", cloudpublisher.ErrPoolRemapped)}
	s := newRetrySweeperServer(t, st, resumer)

	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{dueRef("run-plain", now.Add(-time.Minute))}}, resumer, now)

	if _, ok := st.abandoned["run-plain"]; !ok {
		t.Fatalf("an unprobed row was not abandoned on a permanent refusal: abandoned=%v probe_rearmed=%v", st.abandoned, st.probeRearmed)
	}
	if len(st.probeRearmed) != 0 {
		t.Errorf("stood down a row that carries no probe marker: %v", st.probeRearmed)
	}
}

// Telling a probed row from an ordinary one is a READ, and the two wrong
// answers are not symmetric: settling a probed row destroys the announced
// instant for good, deferring one costs a tick. So a read that fails defers.
func TestStandDownProbedRetry_AFailedReadDefersInsteadOfSettling(t *testing.T) {
	st := &loadFailingRetryStore{fakeRetryStore: newFakeRetryStore()}
	s := newRetrySweeperServer(t, st, nil)

	if !s.standDownProbedRetry(context.Background(), st, "team-1", "run-unreadable", "admission deferred") {
		t.Fatal("standDownProbedRetry = false when the run doc could not be read — the caller then abandons or re-arms a row that may be a probe's")
	}
	if len(st.abandoned) != 0 || len(st.probeRearmed) != 0 {
		t.Errorf("an unreadable row was written anyway: abandoned=%v probe_rearmed=%v", st.abandoned, st.probeRearmed)
	}
}

// Same asymmetry one step later: the restore itself failing must not let the
// caller settle the row either.
func TestStandDownProbedRetry_AFailedRestoreDefersInsteadOfSettling(t *testing.T) {
	st := newFakeRetryStore()
	now := time.Now().UTC()
	announced := now.Add(4 * 24 * time.Hour)
	st.loadRun["run-probed"] = probedRun("run-probed", now, announced)
	st.rearmErr = errors.New("mongo is down")
	s := newRetrySweeperServer(t, st, nil)

	if !s.standDownProbedRetry(context.Background(), st, "team-1", "run-probed", "admission deferred") {
		t.Fatal("standDownProbedRetry = false when the restore failed — the caller then spends an attempt on a probe's failure")
	}
}

// The max_wait horizon is measured from an anchor, and on a legacy row with
// no scheduled_at that anchor falls back to the ARMED instant — which on a
// probed row is "now". Without the marker a probe would silently re-base the
// horizon on itself; the announced instant it set aside is the row's origin.
func TestCircuitRetryDelayAt_AProbedRowAnchorsOnTheAnnouncedInstant(t *testing.T) {
	now := time.Now().UTC()
	announced := now.Add(4 * 24 * time.Hour)
	ref := probedRef("run-probed", now, announced)
	ref.RetryState.ScheduledAt = nil // a row armed before the anchor existed
	openUntil := now.Add(30 * 24 * time.Hour)
	run := &store.Run{RetryPolicy: &store.RunRetryPolicy{MaxWait: "1h", Jitter: "0s"}}

	delayed, ok := circuitRetryDelayAt(ref, run, &store.RetryCircuitState{OpenUntil: &openUntil}, now)
	if !ok {
		t.Fatal("no delay behind an open circuit")
	}
	if want := announced.Add(time.Hour); !delayed.Equal(want) {
		t.Fatalf("delayed to %s, want the announced instant + max_wait %s — a probe must not move the horizon it was pulled out of",
			delayed.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// The anchor's PRECEDENCE, stated where a reader can see it: a row armed by
// current code always carries scheduled_at (ScheduleRunRetry stamps it
// unconditionally), and the arming instant is the horizon's real origin — so
// the announced-instant branch above is a refinement of the LEGACY fallback
// only, for rows armed before that field existed. Moving it below
// scheduled_at would silently stretch every probed row's max_wait horizon by
// the length of its wait.
func TestCircuitRetryDelayAt_TheArmingInstantOutranksTheAnnouncedOne(t *testing.T) {
	now := time.Now().UTC()
	announced := now.Add(4 * 24 * time.Hour)
	armed := now.Add(-30 * time.Minute)
	ref := probedRef("run-probed", now, announced)
	ref.RetryState.ScheduledAt = &armed
	openUntil := now.Add(30 * 24 * time.Hour)
	run := &store.Run{RetryPolicy: &store.RunRetryPolicy{MaxWait: "1h", Jitter: "0s"}}

	delayed, ok := circuitRetryDelayAt(ref, run, &store.RetryCircuitState{OpenUntil: &openUntil}, now)
	if !ok {
		t.Fatal("no delay behind an open circuit")
	}
	if want := armed.Add(time.Hour); !delayed.Equal(want) {
		t.Fatalf("delayed to %s, want the ARMING instant + max_wait %s: anchoring on the announced instant instead would push the horizon out by the whole wait",
			delayed.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// The settlement guard and the max_wait anchor read the WALL door, which does
// not expire with the pull — and the one state that tells the two doors apart
// is a pull the shared circuit deferred for longer than store.ProbeMarkerTTL.
// Keyed on "who pays" instead, both of these would settle or re-base a row
// whose announced instant is still days ahead: the one irreversible mistake on
// this surface, since the settlement deletes that instant for good.
func TestStandDownProbedRetry_StillProtectsARowWhosePullIsOld(t *testing.T) {
	st := newFakeRetryStore()
	now := time.Now().UTC()
	announced := now.Add(4 * 24 * time.Hour)
	run := probedRun("run-probed", now, announced)
	pulledLongAgo := now.Add(-6 * time.Hour)
	run.RetryState.LastProbeAt = &pulledLongAgo
	st.loadRun["run-probed"] = run
	s := newRetrySweeperServer(t, st, nil)

	s.abandonRetry(context.Background(), st, mongostore.RetryDueRef{ID: "run-probed", TenantID: "team-1"},
		"auto-retry abandoned: whatever the sweeper decided")

	if reason, ok := st.abandoned["run-probed"]; ok {
		t.Fatalf("a row whose pull was %s ago was retired %s before its announced instant (%q): the wall is the row's, not the pull's",
			now.Sub(pulledLongAgo), announced.Sub(now), reason)
	}
	if got, ok := st.probeRearmed["run-probed"]; !ok || !got.Equal(announced) {
		t.Fatalf("the announced instant was not restored (present=%t, at=%v)", ok, got)
	}
}

func TestCircuitRetryDelayAt_AnchorsOnTheAnnouncedInstantWhateverThePullsAge(t *testing.T) {
	now := time.Now().UTC()
	announced := now.Add(4 * 24 * time.Hour)
	ref := probedRef("run-probed", now, announced)
	ref.RetryState.ScheduledAt = nil // a row armed before the anchor existed
	pulledLongAgo := now.Add(-6 * time.Hour)
	ref.RetryState.LastProbeAt = &pulledLongAgo
	openUntil := now.Add(30 * 24 * time.Hour)
	run := &store.Run{RetryPolicy: &store.RunRetryPolicy{MaxWait: "1h", Jitter: "0s"}}

	delayed, ok := circuitRetryDelayAt(ref, run, &store.RetryCircuitState{OpenUntil: &openUntil}, now)
	if !ok {
		t.Fatal("no delay behind an open circuit")
	}
	if want := announced.Add(time.Hour); !delayed.Equal(want) {
		t.Fatalf("delayed to %s, want the announced instant + max_wait %s: measuring the horizon from the probe instead of from the wait it interrupted shortens it by the whole wait",
			delayed.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// The marker's payment window is the probe pace's own default floor, and the
// doc says so (docs/scheduling.md). Pinned here because the two live in
// different packages: a floor raised without the window would charge an
// attempt to every probe the circuit defers past the old hour.
func TestProbeMarkerTTLIsTheDefaultProbeFloor(t *testing.T) {
	if store.ProbeMarkerTTL != defaultRetryProbeFloor {
		t.Fatalf("store.ProbeMarkerTTL = %s, the default probe floor = %s: the window in which a pull can still be paying for a park is the pace that bounds pulls",
			store.ProbeMarkerTTL, defaultRetryProbeFloor)
	}
}

// Disarming the probe (budget 0) stops new pulls; it must NOT retire the rows
// already pulled. A row keeps its marker across an operator's change of mind,
// and the stand-down is what puts the provider's instant back — gating it on
// the resolved budget would settle exactly those rows and delete the instant
// for good, which is the asymmetry this guard exists for.
func TestStandDownProbedRetry_StillProtectsARowPulledBeforeProbingWasDisarmed(t *testing.T) {
	t.Setenv(EnvRetryProbeBudget, "0")
	st := newFakeRetryStore()
	now := time.Now().UTC()
	announced := now.Add(4 * 24 * time.Hour)
	st.loadRun["run-probed"] = probedRun("run-probed", now, announced)
	s := newRetrySweeperServer(t, st, nil)

	s.abandonRetry(context.Background(), st, mongostore.RetryDueRef{ID: "run-probed", TenantID: "team-1"},
		"auto-retry abandoned: whatever the sweeper decided")

	if reason, ok := st.abandoned["run-probed"]; ok {
		t.Fatalf("a probed row was retired %s before its announced instant because probing is now off (%q)", announced.Sub(now), reason)
	}
	if got, ok := st.probeRearmed["run-probed"]; !ok || !got.Equal(announced) {
		t.Fatalf("the announced instant was not restored (present=%t, at=%v)", ok, got)
	}
}

// The probe method rides the SAME capability interface the due scan does, so
// the sweeper starts or stays silent as one (server_lifecycle.go asserts it).
// A Mongo store that stopped satisfying it would stop every usage-window
// retry, not just the probe — assert the capability here rather than
// discover it from a parked fleet.
func TestRetryDueListerIsSatisfiedByTheMongoStore(t *testing.T) {
	var candidate any = (*mongostore.Store)(nil)
	if _, ok := candidate.(retryDueLister); !ok {
		t.Fatal("*mongo.Store no longer satisfies retryDueLister: server_lifecycle.go's assertion fails and NO run parked on a provider quota is ever resumed")
	}
}

func TestRetryProbeBoundsFromEnv_DefaultsOverridesAndTypos(t *testing.T) {
	if got := retryProbeBoundsFromEnv(); got.floor != defaultRetryProbeFloor || got.budget != defaultRetryProbeBudget {
		t.Fatalf("defaults = %+v, want floor %s budget %d", got, defaultRetryProbeFloor, defaultRetryProbeBudget)
	}
	t.Setenv(EnvRetryProbeFloor, "4h")
	t.Setenv(EnvRetryProbeBudget, "6")
	if got := retryProbeBoundsFromEnv(); got.floor != 4*time.Hour || got.budget != 6 {
		t.Fatalf("overrides = %+v, want floor 4h budget 6", got)
	}
	// A typo must not turn the floor into "every tick"; 0 stays 0, because
	// "off" is a value an operator means.
	t.Setenv(EnvRetryProbeFloor, "soon")
	t.Setenv(EnvRetryProbeBudget, "0")
	if got := retryProbeBoundsFromEnv(); got.floor != defaultRetryProbeFloor || got.budget != 0 {
		t.Fatalf("typo + off = %+v, want the default floor and budget 0", got)
	}
	t.Setenv(EnvRetryProbeBudget, "-1")
	if got := retryProbeBoundsFromEnv(); got.budget != defaultRetryProbeBudget {
		t.Fatalf("budget -1 = %d, want the default %d", got.budget, defaultRetryProbeBudget)
	}
}

// loadFailingRetryStore is the store that cannot answer "is this row a
// probe's".
type loadFailingRetryStore struct{ *fakeRetryStore }

func (l *loadFailingRetryStore) LoadRun(context.Context, string) (*store.Run, error) {
	return nil, errors.New("mongo is down")
}

// And what ENDS that refund for good. The marker outlives the resume it was
// written for by design, and only an arming deletes it: a probe-driven resume
// that never parks leaves it on the row. Every later resume of that row would
// then be refunded to a probe that finished days ago — the under-count would
// be unbounded instead of the one run per wait the design allows.
func TestSweepDueRetries_AResumeOnAMarkerFromAFinishedWaitIsBillable(t *testing.T) {
	st := newFakeRetryStore()
	s, counter := newMeteredRetrySweeperServer(t, st)
	resumer := &fakeResumer{}

	now := time.Now().UTC()
	st.claimWins["run-probed"] = true
	// The instant that probe held aside fell four days ago: nothing is
	// waiting on it, and this row is due on its own account.
	s.sweepDueRetries(context.Background(),
		&fakeRetryLister{refs: []mongostore.RetryDueRef{probedRef("run-probed", now, now.Add(-4*24*time.Hour))}}, resumer, now)

	if len(resumer.calls) != 1 {
		t.Fatalf("Resume called %d times, want 1", len(resumer.calls))
	}
	if got := meteredRetryRuns(t, counter); got != 1 {
		t.Fatalf("the resume metered %d monthly run-quota units, want 1: a marker whose wait is over names no probe in flight, so this is an ordinary billable launch", got)
	}
}

// The third reading of the same door, and the one an operator pays for: a
// wall still days ahead, a PULL older than store.ProbeMarkerTTL. More than an
// hour between the pull and the resume the due pass reaches — a shared
// circuit that deferred it, a sweeper outage, a pod slow to its preflight —
// and that resume is nobody's probe: nothing refunds it, so the tenant's
// monthly run quota pays for it, and the park it causes is charged an attempt
// (pkg/runner, TestArmUsageWindowRetry_AStaleMarkerParkIsChargedAndKeepsTheWallsProbeLedger).
// The mild error store.ProbeMarkerTTL names, measured here because the pages
// that size the probe's cost for an operator state it.
func TestSweepDueRetries_AResumeOlderThanTheMarkerTTLIsMetered(t *testing.T) {
	st := newFakeRetryStore()
	s, counter := newMeteredRetrySweeperServer(t, st)
	resumer := &fakeResumer{}
	now := time.Now().UTC()
	wall := now.Add(4 * 24 * time.Hour)

	// The pull the probe pass took a minute ago: that resume IS the probe's,
	// and the metered unit is handed back.
	st.claimWins["run-fresh"] = true
	s.sweepDueRetries(context.Background(),
		&fakeRetryLister{refs: []mongostore.RetryDueRef{probedRef("run-fresh", now.Add(-time.Minute), wall)}}, resumer, now)
	if got := meteredRetryRuns(t, counter); got != 0 {
		t.Fatalf("the resume of a one-minute-old pull metered %d monthly run-quota units, want 0", got)
	}

	// The same row shape and the same wall, with a pull two hours old.
	st.claimWins["run-stale"] = true
	s.sweepDueRetries(context.Background(),
		&fakeRetryLister{refs: []mongostore.RetryDueRef{probedRef("run-stale", now.Add(-2*store.ProbeMarkerTTL), wall)}}, resumer, now)
	if got := meteredRetryRuns(t, counter); got != 1 {
		t.Fatalf("the resume of a pull %s old metered %d monthly run-quota units, want 1: the WALL door still holds the announced instant (%s away), but the PAYMENT door expires with the pull, so this launch is the tenant's — at the shipped max_attempts one wait can spend 5 of them",
			2*store.ProbeMarkerTTL, got, time.Until(wall).Round(time.Hour))
	}
	if len(resumer.calls) != 2 {
		t.Fatalf("Resume called %d times, want 2 — both rows reached the resume", len(resumer.calls))
	}
}

// The same expiry on the settlement side: restoring an instant already behind
// us arms the row in the past, so the next pass resumes it again — a spin at
// the sweeper's cadence in place of the caller's backoff, and a run that can
// never be retired.
func TestStandDownProbedRetry_AMarkerFromAFinishedWaitLeavesTheSettlement(t *testing.T) {
	st := newFakeRetryStore()
	now := time.Now().UTC()
	st.loadRun["run-probed"] = probedRun("run-probed", now, now.Add(-4*24*time.Hour))
	s := newRetrySweeperServer(t, st, nil)

	if s.standDownProbedRetry(context.Background(), st, "team-1", "run-probed", "admission deferred") {
		t.Fatal("the stand-down took a row whose held-aside instant has already passed — the caller's ordinary re-arm and backoff never run")
	}
	if len(st.probeRearmed) != 0 {
		t.Fatalf("the row was re-armed in the past: %v", st.probeRearmed)
	}
}

// A row whose document is gone is nobody's probe either: the caller settles
// it as it would any other, and nothing is written on a run that no longer
// exists. (A read ERROR is the opposite case — it defers; the test above
// pins that asymmetry.)
func TestStandDownProbedRetry_AMissingRunDocIsNotAProbe(t *testing.T) {
	st := newFakeRetryStore()
	st.loadRun["run-gone"] = nil
	s := newRetrySweeperServer(t, st, nil)

	if s.standDownProbedRetry(context.Background(), st, "team-1", "run-gone", "admission deferred") {
		t.Fatal("the stand-down took a run with no document — the caller's ordinary settlement never runs")
	}
	if len(st.probeRearmed) != 0 {
		t.Fatalf("a run with no document was re-armed: %v", st.probeRearmed)
	}
}

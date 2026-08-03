package credpool

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Regressions for defects an adversarial review found in the accounting.
// Each one silently spent or withheld a contributor's quota, which is the
// only kind of bug this feature cannot ship with.

// A run can be reported twice — a redelivery whose first pod was merely
// slow, a cancel racing a finish. Reading "still open" and then charging
// debits the donor once per report; the close CAS has to be the arbiter.
func TestReport_secondReportDoesNotChargeAgain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10})
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Two reports racing: both observed the lease open before either
	// closed it. Simulated by reporting twice — the first closes.
	for i := 0; i < 2; i++ {
		if err := h.broker.Report(ctx, "run-1", Outcome{CostUSD: 2}); err != nil {
			t.Fatalf("Report %d: %v", i, err)
		}
	}
	day, _, _ := h.ledger.Usage(ctx, PledgeID("alice", "claude_code"), h.now)
	if day.CostUSD != 2 {
		t.Errorf("donor charged %v, want 2 — a second report double-charged them", day.CostUSD)
	}
}

// A run that resumes is the SAME run. Counting a fresh unit of "runs per
// day" for every attempt let one flaky run eat a contributor's whole day.
func TestAcquire_resumeDoesNotConsumeASecondRunUnit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxRunsPerDay: 2})

	// One run, admitted then resumed three times.
	for i := 0; i < 4; i++ {
		if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
			t.Fatalf("acquire attempt %d: %v", i, err)
		}
	}
	day, _, _ := h.ledger.Usage(ctx, PledgeID("alice", "claude_code"), h.now)
	if day.Runs != 1 {
		t.Errorf("day runs = %d, want 1 — resuming re-consumed the donor's run quota", day.Runs)
	}
	// And the donor's second genuine run is still available to someone else.
	if _, err := h.broker.Acquire(ctx, h.request("run-2")); err != nil {
		t.Errorf("a genuinely new run was refused: %v", err)
	}
}

// Concurrent launches used to each read the same "remaining" and each be
// granted it in full: N runs could spend the daily cap N times over. The
// allowance already promised to live leases must count against the next.
func TestAcquire_inFlightAllowanceIsNotHandedOutTwice(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 5})

	first, err := h.broker.Acquire(ctx, h.request("run-1"))
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if first.RemainingUSD != 5 {
		t.Fatalf("first allowance = %v, want the full 5", first.RemainingUSD)
	}
	// Nothing has been REPORTED yet — the whole cap is promised to run-1.
	if _, err := h.broker.Acquire(ctx, h.request("run-2")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("second Acquire = %v, want ErrNoDonor — the donor's cap was promised twice", err)
	}

	// Once run-1 reports well under its allowance, the rest is lendable.
	if err := h.broker.Report(ctx, "run-1", Outcome{CostUSD: 1}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	third, err := h.broker.Acquire(ctx, h.request("run-3"))
	if err != nil {
		t.Fatalf("third Acquire: %v", err)
	}
	if third.RemainingUSD != 4 {
		t.Errorf("allowance after a $1 run = %v, want 4", third.RemainingUSD)
	}
}

// An exhausted donor must be refused, never granted an allowance of 0 —
// zero on the wire means "no ceiling", so handing it out would turn a
// spent contributor into an unlimited one.
func TestAcquire_exhaustedDonorIsRefusedNotGrantedZero(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 5})
	if err := h.ledger.AddSpend(ctx, PledgeID("alice", "claude_code"), h.now, 5, 0, 0); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
	grant, err := h.broker.Acquire(ctx, h.request("run-1"))
	if !errors.Is(err, ErrNoDonor) {
		t.Fatalf("Acquire = (%+v, %v), want ErrNoDonor", grant, err)
	}
}

// A launch that fails after the credential was granted must give the
// admission back: the run never happened, so it must not cost the donor a
// slot, an allowance, or a unit of their daily quota.
func TestRelease_returnsAnAdmissionWhoseRunNeverStarted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxRunsPerDay: 1, MaxUSDPerDay: 5, MaxConcurrentRuns: 1})

	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	h.broker.Release(ctx, "run-1")

	day, _, _ := h.ledger.Usage(ctx, PledgeID("alice", "claude_code"), h.now)
	if day.Runs != 0 {
		t.Errorf("day runs = %d, want 0 — the donor paid for a run that never launched", day.Runs)
	}
	grant, err := h.broker.Acquire(ctx, h.request("run-2"))
	if err != nil {
		t.Fatalf("the donor stayed locked out after a failed launch: %v", err)
	}
	if grant.RemainingUSD != 5 {
		t.Errorf("allowance = %v, want the full 5 back", grant.RemainingUSD)
	}
}

// Release is called from error paths that must surface their own error, so
// it has to tolerate every shape of "nothing to release".
func TestRelease_isSafeOnAnythingElse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{})
	h.broker.Release(ctx, "")            // no run
	h.broker.Release(ctx, "run-unknown") // never acquired
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := h.broker.Report(ctx, "run-1", Outcome{CostUSD: 1}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	h.broker.Release(ctx, "run-1") // already closed — must not refund
	day, _, _ := h.ledger.Usage(ctx, PledgeID("alice", "claude_code"), h.now)
	if day.Runs != 1 {
		t.Errorf("day runs = %d, want 1 — a released-after-report run gave back a unit it had used", day.Runs)
	}
}

// The donor's record of a run is the only place their charge is
// explainable. A resume onto a DIFFERENT donor used to overwrite it,
// leaving money on the first donor's ledger with nothing to account for it.
func TestAcquire_resumeOnAnotherDonorKeepsTheFirstOnesRecord(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 5})
	h.donor(t, "bob", Limits{MaxUSDPerDay: 5})

	first, err := h.broker.Acquire(ctx, h.request("run-1"))
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if err := h.broker.Report(ctx, "run-1", Outcome{
		CostUSD: 2, Condition: ConditionUsageWindow, CooldownUntil: h.now.Add(3 * time.Hour),
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	// The first donor is now resting, so the resume lands on the other.
	second, err := h.broker.Acquire(ctx, h.request("run-1"))
	if err != nil {
		t.Fatalf("resume Acquire: %v", err)
	}
	if second.DonorID == first.DonorID {
		t.Fatalf("resume picked the resting donor %q", second.DonorID)
	}

	// The first donor's history must still explain their $2.
	history, err := h.leases.ListByDonor(ctx, first.DonorID, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("first donor has %d lease(s), want 1 — their record was overwritten", len(history))
	}
	if history[0].CostUSD != 2 || history[0].RunID != "run-1" {
		t.Errorf("record = (run %q, $%v), want (run-1, $2)", history[0].RunID, history[0].CostUSD)
	}
	day, _, _ := h.ledger.Usage(ctx, PledgeID(first.DonorID, "claude_code"), h.now)
	if day.CostUSD != 2 {
		t.Errorf("first donor's ledger = %v, want 2 (and it must match their history)", day.CostUSD)
	}
}

// Reporting a run whose lease moved to another donor must charge the donor
// that actually served the attempt.
func TestReport_chargesTheDonorServingTheCurrentAttempt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 5})
	h.donor(t, "bob", Limits{MaxUSDPerDay: 5})

	first, _ := h.broker.Acquire(ctx, h.request("run-1"))
	_ = h.broker.Report(ctx, "run-1", Outcome{
		CostUSD: 1, Condition: ConditionUsageWindow, CooldownUntil: h.now.Add(3 * time.Hour),
	})
	second, err := h.broker.Acquire(ctx, h.request("run-1"))
	if err != nil {
		t.Fatalf("resume Acquire: %v", err)
	}
	if err := h.broker.Report(ctx, "run-1", Outcome{CostUSD: 3}); err != nil {
		t.Fatalf("second Report: %v", err)
	}

	firstDay, _, _ := h.ledger.Usage(ctx, PledgeID(first.DonorID, "claude_code"), h.now)
	secondDay, _, _ := h.ledger.Usage(ctx, PledgeID(second.DonorID, "claude_code"), h.now)
	if firstDay.CostUSD != 1 {
		t.Errorf("%s charged %v, want 1 (only their own attempt)", first.DonorID, firstDay.CostUSD)
	}
	if secondDay.CostUSD != 3 {
		t.Errorf("%s charged %v, want 3 (the attempt they served)", second.DonorID, secondDay.CostUSD)
	}
}

// The sweeper frees an abandoned run's slot but must NOT refund its run
// unit: that run was served, whatever it failed to report.
func TestReleaseExpired_keepsTheRunUnitOfAServedRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxRunsPerDay: 2, MaxConcurrentRuns: 1})
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	h.now = h.now.Add(DefaultLeaseTTL + time.Minute)
	if _, err := h.broker.ReleaseExpired(ctx, 10); err != nil {
		t.Fatalf("ReleaseExpired: %v", err)
	}
	day, _, _ := h.ledger.Usage(ctx, PledgeID("alice", "claude_code"), h.now.Add(-DefaultLeaseTTL-time.Minute))
	if day.Runs != 1 {
		t.Errorf("day runs = %d, want 1 — a served run's quota was refunded", day.Runs)
	}
}

// A donor serving a resumed run must not be blocked by their own
// concurrency cap: the run holds one lease, which is its own.
func TestAcquire_resumeIsNotBlockedByItsOwnConcurrencySlot(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxConcurrentRuns: 1})
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Errorf("resume was refused by its own lease: %v", err)
	}
}

func TestLimitsDeny_countsCommittedSpend(t *testing.T) {
	lim := Limits{MaxUSDPerDay: 10}
	// $6 spent, $5 promised to a live run → the next admission would put
	// the donor over, so it must be refused rather than granted $4.
	if _, deny := decide(lim, 1, 6, 0, LiveCommitment{CommittedUSD: 5}); deny != DenyCostPerDay {
		t.Errorf("deny = %q, want cost_per_day", deny)
	}
	if remaining, deny := decide(lim, 1, 6, 0, LiveCommitment{}); deny != DenyNone || remaining != 4 {
		t.Errorf("decide = (%v, %q), want (4, none)", remaining, deny)
	}
}

func TestBrokerRelease_nilIsSafe(t *testing.T) {
	var b *Broker
	b.Release(context.Background(), "run-1")
}

// Round-2 regressions: defects found in the round-1 FIXES themselves.

// A pod killed mid-run leaves its lease open. If the resume then acquires a
// different donor, the run holds two open leases and "who is serving it" —
// hence who gets charged — was decided by whichever the store happened to
// return first (a randomised map iteration in memory). Acquiring must
// supersede what it replaces.
func TestAcquire_supersedesAnOrphanedOpenLease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 5})
	h.donor(t, "bob", Limits{MaxUSDPerDay: 5})

	first, err := h.broker.Acquire(ctx, h.request("run-1"))
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	// No Report — the pod died. Park the first donor so the resume must
	// pick the other one.
	p, _ := h.pledges.Get(ctx, PledgeID(first.DonorID, "claude_code"))
	p.Health = HealthAuthFailed
	if err := h.pledges.Upsert(ctx, p); err != nil {
		t.Fatalf("park: %v", err)
	}
	second, err := h.broker.Acquire(ctx, h.request("run-1"))
	if err != nil {
		t.Fatalf("resume Acquire: %v", err)
	}
	if second.DonorID == first.DonorID {
		t.Fatalf("resume picked the parked donor %q", second.DonorID)
	}

	open, err := h.leases.ListOpenByRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("run-1 holds %d open leases, want 1 — the charged donor would be arbitrary", len(open))
	}
	if open[0].DonorID != second.DonorID {
		t.Errorf("open lease belongs to %q, want the donor now serving (%q)", open[0].DonorID, second.DonorID)
	}

	// And the report charges that donor, not the orphan.
	if err := h.broker.Report(ctx, "run-1", Outcome{CostUSD: 3}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	orphan, _, _ := h.ledger.Usage(ctx, PledgeID(first.DonorID, "claude_code"), h.now)
	serving, _, _ := h.ledger.Usage(ctx, PledgeID(second.DonorID, "claude_code"), h.now)
	if orphan.CostUSD != 0 {
		t.Errorf("%s was charged %v for work they did not serve", first.DonorID, orphan.CostUSD)
	}
	if serving.CostUSD != 3 {
		t.Errorf("%s charged %v, want 3", second.DonorID, serving.CostUSD)
	}
}

// Re-admitting a donor used to overwrite the finished attempt's record,
// which both erased the evidence for a charge already on their ledger and
// re-opened the lease — re-arming the close CAS so a redelivered report
// could charge a second time.
func TestAcquire_readmissionKeepsTheFinishedAttemptsRecord(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10})

	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := h.broker.Report(ctx, "run-1", Outcome{CostUSD: 2}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("resume Acquire: %v", err)
	}

	hist, _ := h.leases.ListByDonor(ctx, "alice", 10)
	if len(hist) != 2 {
		t.Fatalf("history = %d lease(s), want 2 (one per attempt)", len(hist))
	}
	var charged int
	for _, l := range hist {
		if l.Closed && l.CostUSD == 2 {
			charged++
		}
	}
	if charged != 1 {
		t.Errorf("the finished attempt's $2 record is gone — the donor's ledger no longer reconciles")
	}

	// A redelivery of the FIRST attempt must not charge again.
	if err := h.broker.Report(ctx, "run-1", Outcome{CostUSD: 2}); err != nil {
		t.Fatalf("second Report: %v", err)
	}
	day, _, _ := h.ledger.Usage(ctx, PledgeID("alice", "claude_code"), h.now)
	if day.CostUSD != 4 {
		// 2 (attempt 1) + 2 (attempt 2, legitimately reported) = 4.
		t.Errorf("charged %v, want 4", day.CostUSD)
	}
}

// Releasing a RESUME must not hand back a run unit it never took: that
// would mint quota out of a failed launch and let the donor be drawn on
// past the ceiling they set.
func TestRelease_doesNotRefundARenewedAdmission(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxRunsPerDay: 1})

	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil { // resume
		t.Fatalf("resume Acquire: %v", err)
	}
	h.broker.Release(ctx, "run-1") // the resume's launch failed

	day, _, _ := h.ledger.Usage(ctx, PledgeID("alice", "claude_code"), h.now)
	if day.Runs != 1 {
		t.Errorf("day runs = %d, want 1 — releasing a resume refunded a unit it never took", day.Runs)
	}
	// The donor's 1-run-per-day ceiling still holds.
	if _, err := h.broker.Acquire(ctx, h.request("run-2")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("a second run was admitted past a MaxRunsPerDay=1 pledge: %v", err)
	}
}

// A run whose attempts keep being abandoned (crash-looping pod) must be
// charged as new each time, not renew forever against a record that never
// learned what it spent.
func TestAcquire_abandonedAttemptDoesNotEarnAFreeReadmission(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxRunsPerDay: 2})

	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	h.now = h.now.Add(DefaultLeaseTTL + time.Minute)
	if _, err := h.broker.ReleaseExpired(ctx, 10); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Same run, new attempt: the previous one told us nothing, so it counts.
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	day, _, _ := h.ledger.Usage(ctx, PledgeID("alice", "claude_code"), h.now)
	if day.Runs != 1 {
		t.Fatalf("day runs = %d on the new day, want 1", day.Runs)
	}
	prior, _, _ := h.ledger.Usage(ctx, PledgeID("alice", "claude_code"), h.now.Add(-DefaultLeaseTTL-time.Minute))
	if prior.Runs != 1 {
		t.Errorf("the abandoned attempt's unit = %d, want 1 (it was served)", prior.Runs)
	}
}

// The memory store is the semantic reference; an empty excludeRunID must
// mean "exclude nothing", not "skip every lease with an empty run id".
func TestLiveCommitment_emptyExcludeCountsEverything(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 5})
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	runs, committed, err := h.leases.LiveCommitment(ctx, PledgeID("alice", "claude_code"), "", h.now)
	if err != nil {
		t.Fatalf("LiveCommitment: %v", err)
	}
	if runs != 1 || committed != 5 {
		t.Errorf("LiveCommitment = (%d, %v), want (1, 5)", runs, committed)
	}
}

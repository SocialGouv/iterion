package server

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/retrypolicy"
)

// The outage class this net exists for is a provider usage window, and a
// weekly one shuts for DAYS. An hour-long reach survives a dropped event; it
// does not survive the thing that kills reviews in batches.
//
// Measured 2026-09-15 on the production deployment: seven runs died on one
// weekly cap, two of them gating runs still holding valid publish grants, and
// their required checks sat `pending` for 81 hours — every pass that could
// have answered them had stopped 80 hours earlier.
func TestTheSweepHorizonOutlivesAProviderUsageWindow(t *testing.T) {
	const aWeeklyWindow = 7 * 24 * time.Hour
	if gateSweepHorizon < aWeeklyWindow {
		t.Errorf("horizon %s is shorter than the %s a weekly provider window can shut for — every check a capped run owes goes unanswered past it",
			gateSweepHorizon, aWeeklyWindow)
	}
	// Anchored on the retry policy rather than restated: a run is allowed to
	// sit waiting for its window that long, so the net that answers for it has
	// to outlast the same wait.
	if gateSweepHorizon != retrypolicy.DefaultMaxWait {
		t.Errorf("horizon %s has drifted from the longest wait the retry policy permits (%s)",
			gateSweepHorizon, retrypolicy.DefaultMaxWait)
	}
}

// The coupling that decides whether ANY of this works. The reconciler needs
// the run's publish grant to know which repo and connection to speak through,
// and abstains with "its publish grant is expired or revoked" without one. A
// grant that dies before the horizon turns every later pass into a guaranteed
// abstain — a net that reads, from the outside, exactly like one still trying.
//
// NECESSARY, NOT SUFFICIENT — and the difference cost this branch its central
// claim for a while. Two durations can sit in the right order and still
// describe grants that die mid-window, because they are measured from
// DIFFERENT instants: the grant's expiry from LAUNCH (Register), the sweep's
// candidacy from the run's TERMINAL updated_at. For a run parked a week on a
// usage window those are a week apart, and this comparison cannot see it.
// What actually holds the coupling up is the re-anchoring in
// expireForgePublishGrantForRun, pinned by walking a clock across both
// instants in TestAGrantOutlivesTheHorizonEvenWhenTheRunParkedForAWeekFirst.
// This one stays because it fails faster and names the constant that drifted.
func TestThePublishGrantOutlivesTheSweepHorizon(t *testing.T) {
	if forgePublishPostRunGrace <= gateSweepHorizon {
		t.Fatalf("grant lives %s after the run but the net keeps offering it until %s — the passes in between can only abstain",
			forgePublishPostRunGrace, gateSweepHorizon)
	}
	// And the shortened post-run life must still fit inside the absolute TTL,
	// or the grant is revoked by the other end before its own grace expires.
	if forgePublishPostRunGrace > forgePublishDefaultTTL {
		t.Errorf("post-run grace %s exceeds the grant's own TTL %s", forgePublishPostRunGrace, forgePublishDefaultTTL)
	}
	// The horizon-long life is for the runs that CLAIMED a check. Everything
	// else is retired on the ordinary window, which is what keeps terminal
	// eviction doing the job forgePublishMaxTokens is sized against.
	if forgePublishDeadRunGrace >= forgePublishPostRunGrace {
		t.Errorf("a dead run that claims no gate keeps its grant for %s, as long as one that owes a verdict — then nothing is evicted early and the registry's cap is really 'grants per TTL'",
			forgePublishDeadRunGrace)
	}
}

// The two-tier rule itself. Keeping the every-minute pass narrow is what makes
// a horizon of days affordable; making every Nth pass deep is what makes the
// horizon real rather than declared.
func TestOnlyTheDeepPassReachesTheHorizon(t *testing.T) {
	if got := gateSweepWindowFor(0); got != gateSweepHorizon {
		t.Errorf("the first pass reaches %s, want the full %s — a replica that just started is the one that missed the most", got, gateSweepHorizon)
	}
	if got := gateSweepWindowFor(1); got != gateSweepLookback {
		t.Errorf("an ordinary pass reaches %s, want %s", got, gateSweepLookback)
	}
	if got := gateSweepWindowFor(gateDeepSweepEvery); got != gateSweepHorizon {
		t.Errorf("pass %d reaches %s, want the full %s", gateDeepSweepEvery, got, gateSweepHorizon)
	}
	deep := 0
	for pass := 0; pass < 4*gateDeepSweepEvery; pass++ {
		if gateSweepWindowFor(pass) == gateSweepHorizon {
			deep++
		}
	}
	if deep != 4 {
		t.Errorf("%d deep passes in %d, want 4 — the cadence decides both the cost and the recovery latency", deep, 4*gateDeepSweepEvery)
	}
}

// What the window choice actually buys, read at the boundary the store query
// is given: a run that died three days into a capped week is inside the deep
// pass's reach and outside the fast one's. Asserted on sweepGates itself, not
// on the constants, so a change that keeps the numbers and breaks the wiring
// still fails here.
func TestTheDeepPassScansBackPastAMultiDayOutage(t *testing.T) {
	s, _ := gateReconcileFixture(t, gatingInputs(), &listingGateClient{})
	now := time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC)
	diedAt := now.Add(-72 * time.Hour)

	fast := &fakeGateSweepLister{}
	s.sweepGates(context.Background(), fast, now, gateSweepWindowFor(1), time.Time{})
	if !fast.since.After(diedAt) {
		t.Errorf("the fast pass reached back to %s, past a run that died at %s — then it is not the narrow pass the cadence assumes", fast.since, diedAt)
	}

	deep := &fakeGateSweepLister{}
	s.sweepGates(context.Background(), deep, now, gateSweepWindowFor(0), time.Time{})
	if deep.since.After(diedAt) {
		t.Errorf("the deep pass reached back only to %s, so a run that died at %s is never offered again — the 81-hour pending check reproduces", deep.since, diedAt)
	}
}

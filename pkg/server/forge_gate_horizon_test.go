package server

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
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
func TestTheGateGrantOutlivesTheSweepHorizon(t *testing.T) {
	if forgePublishGateGrace <= gateSweepHorizon {
		t.Fatalf("a gating run's grant lives %s after the run but the net keeps offering it until %s — the passes in between can only abstain",
			forgePublishGateGrace, gateSweepHorizon)
	}
	// And it must still fit inside the absolute TTL, or the grant is revoked
	// by the other end before its own grace expires.
	if forgePublishGateGrace > forgePublishDefaultTTL {
		t.Errorf("gate grace %s exceeds the grant's own TTL %s", forgePublishGateGrace, forgePublishDefaultTTL)
	}
}

// ...and the other half of the same decision. The horizon-long grant is for
// the runs a repair may still need; handing it to EVERY run that ever held a
// pr_url would falsify the argument forgePublishMaxTokens is sized on — that
// terminal eviction holds the steady state near "gating launches in flight"
// rather than "per TTL" — and on the in-memory registry saturation refuses a
// launch outright, which is a worse outcome than the stuck check being fixed.
// It would also keep a crashed run's forge-write token live for days against
// a TTL that justifies itself as short enough for a leaked one to expire.
func TestAnOrdinaryRunsGrantIsStillRetiredPromptly(t *testing.T) {
	if forgePublishPostRunGrace >= gateSweepHorizon {
		t.Errorf("every run's grant now lives %s — the eviction the token cap is sized against no longer happens", forgePublishPostRunGrace)
	}
	if forgePublishPostRunGrace <= gateSweepLookback {
		t.Errorf("ordinary grace %s does not outlast the fast pass's own %s window — the repair would race its own credential",
			forgePublishPostRunGrace, gateSweepLookback)
	}
}

// And the predicate that routes between the two is the reconciler's own, not a
// second copy of it: the credential is kept exactly as long as the thing that
// needs it says a repair may still happen.
func TestTheGraceFollowsWhoActuallyOwesAVerdict(t *testing.T) {
	gating := &store.Run{Inputs: map[string]any{"gate_context": "revi/review"}}
	if !runOwesGateVerdict(gating) {
		t.Error("a run carrying the repo's gate context owes a verdict")
	}
	// Holding a publish grant is not owing a verdict — the brancher, the
	// amender and the implementer all get one from a bare pr_url.
	if runOwesGateVerdict(&store.Run{Inputs: map[string]any{"pr_url": "https://github.com/o/r/pull/42"}}) {
		t.Error("a run with no gate context owes nothing, and must not hold a horizon-long credential")
	}
	if runOwesGateVerdict(nil) {
		t.Error("a run that could not be read owes nothing")
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

// The constants above are only a promise; this is the reaper actually keeping
// it. A dead gating run must still hold a usable grant deep into the horizon —
// that grant is what the repair speaks through — while a run that owes no
// verdict is retired on the old short window, so the token cap keeps its shape
// and a crashed run's forge-write credential does not linger for days.
func TestTheReaperKeepsAGatingRunsGrantAndRetiresTheRest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inputs   map[string]any
		wantLong bool
	}{
		{"owes a gate verdict", map[string]any{"gate_context": "revi/review"}, true},
		{"holds a grant but gates nothing", map[string]any{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s, _ := newForgePublishTestServer(t)
			s.cfg.Store = st
			const token = "tok-horizon"
			registerPublishToken(t, s, token, ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})

			inputs := map[string]any{
				forgePublishVarToken: token,
				"pr_url":             "https://github.com/o/r/pull/42",
			}
			for k, v := range tc.inputs {
				inputs[k] = v
			}
			if _, err := st.CreateRun(context.Background(), "run-horizon", "review_pr", inputs); err != nil {
				t.Fatal(err)
			}
			run, err := st.LoadRun(context.Background(), "run-horizon")
			if err != nil {
				t.Fatal(err)
			}
			run.Status = store.RunStatusFailedResumable
			run.FailureCode = "USAGE_LIMIT_BLOCKED"
			run.RetryState = nil // no armed retry: the shape that died on the cap
			if err := st.SaveRun(context.Background(), run); err != nil {
				t.Fatal(err)
			}

			if err := s.expireForgePublishGrantForRun(context.Background(), "run-horizon"); err != nil {
				t.Fatal(err)
			}
			g, ok := s.forgePublishTokens.lookup(token)
			if !ok {
				t.Fatal("the grant must stay usable through the repair window, not vanish")
			}
			left := time.Until(g.ExpiresAt)
			if tc.wantLong && left <= gateSweepHorizon {
				t.Errorf("a gating run's grant was cut to %s, inside the %s the net keeps offering it — every later pass can only abstain", left, gateSweepHorizon)
			}
			if !tc.wantLong && left > forgePublishPostRunGrace+time.Minute {
				t.Errorf("a run that owes no verdict kept its grant for %s, want ≤ %s — the eviction the token cap is sized against", left, forgePublishPostRunGrace)
			}
		})
	}
}

// fullPageLister always hands back a complete page whose rows step backwards
// from the cursor, so every pass runs out of page budget. It is the shape a
// deployment with more notifiable runs than gateSweepBatch×gateSweepMaxPages
// presents — including the paused_waiting_human backlog the query returns with
// no time bound at all.
type fullPageLister struct {
	firstBefore time.Time
	lastBefore  time.Time
	calls       int
}

func (l *fullPageLister) ListNotifiableRuns(_ context.Context, _, before time.Time, limit int) ([]mongostore.NotifiableRunRef, error) {
	if l.calls == 0 {
		l.firstBefore = before
	}
	l.lastBefore = before
	l.calls++
	out := make([]mongostore.NotifiableRunRef, 0, limit)
	for i := 1; i <= limit; i++ {
		out = append(out, mongostore.NotifiableRunRef{
			ID:        "absent-run",
			UpdatedAt: before.Add(-time.Duration(i) * time.Second),
		})
	}
	return out, nil
}

// The horizon is a promise about REACH, and a pass capped at
// gateSweepMaxPages×gateSweepBatch rows cannot keep it alone: rows arrive
// newest-first, so a deep pass restarting at now−grace every time would
// re-examine the same newest rows forever and never reach the old ones —
// which are exactly the batch-death runs the horizon exists for. Starving the
// oldest is the precise failure the widened window was supposed to end.
func TestSuccessiveDeepPassesDescendInsteadOfRescanningTheNewest(t *testing.T) {
	s, _ := gateReconcileFixture(t, gatingInputs(), &listingGateClient{})
	now := time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC)

	first := &fullPageLister{}
	cursor := s.sweepGates(context.Background(), first, now, gateSweepHorizon, time.Time{})
	if cursor.IsZero() {
		t.Fatal("a pass that ran out of page budget returned no cursor — the next one restarts at the newest rows and the oldest are never examined")
	}
	if !cursor.Before(first.firstBefore) {
		t.Fatalf("cursor %s did not descend below the pass's start %s", cursor, first.firstBefore)
	}

	second := &fullPageLister{}
	next := s.sweepGates(context.Background(), second, now, gateSweepHorizon, cursor)
	if !second.firstBefore.Equal(cursor) {
		t.Errorf("the second pass started at %s, want the cursor %s — without resuming it re-walks ground already covered", second.firstBefore, cursor)
	}
	if !next.Before(cursor) {
		t.Errorf("the second pass ended at %s, not older than %s — successive passes must make progress toward the horizon", next, cursor)
	}
}

// The descent has to end somewhere or the deep lane parks in the past and
// stops covering anything. A cursor that has fallen out of the (sliding)
// window means the horizon was traversed: start over at the newest end.
func TestADeepCursorPastTheWindowStartsOverAtTheNewestEnd(t *testing.T) {
	s, _ := gateReconcileFixture(t, gatingInputs(), &listingGateClient{})
	now := time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC)
	stale := now.Add(-gateSweepHorizon - 48*time.Hour)

	l := &fakeGateSweepLister{}
	s.sweepGates(context.Background(), l, now, gateSweepHorizon, stale)

	if want := now.Add(-gateSweepGrace); !l.before.Equal(want) {
		t.Errorf("resumed at %s from a cursor already past the window, want a fresh start at %s", l.before, want)
	}
}

// A pass whose cursor did not move must not be resumed from: every subsequent
// pass would ask the same question and get the same page, which is the stall
// the in-pass guard already names — turned permanent by a saved cursor.
func TestAStalledCursorIsNotCarriedIntoTheNextPass(t *testing.T) {
	s, _ := gateReconcileFixture(t, gatingInputs(), &listingGateClient{})
	now := time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC)

	// Every row carries the same instant, so `oldest` can never advance.
	frozen := &frozenPageLister{at: now.Add(-2 * time.Hour)}
	if got := s.sweepGates(context.Background(), frozen, now, gateSweepHorizon, time.Time{}); !got.IsZero() {
		t.Errorf("returned cursor %s after a stall — the next pass would re-ask the identical question forever", got)
	}
}

type frozenPageLister struct{ at time.Time }

func (l *frozenPageLister) ListNotifiableRuns(_ context.Context, _, _ time.Time, limit int) ([]mongostore.NotifiableRunRef, error) {
	out := make([]mongostore.NotifiableRunRef, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, mongostore.NotifiableRunRef{ID: "absent-run", UpdatedAt: l.at})
	}
	return out, nil
}

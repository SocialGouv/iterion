package server

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// abstainingSweepFixture builds a run that owes a gate verdict but whose
// publish grant cannot be resolved — the reconciler's first abstain branch,
// and the shape of a run stuck in a PERMANENT abstain: every sweep pass walks
// the same branch and declines, for the whole lookback, then never again.
//
// The logger is pinned at warn, which is the level deployments run at (prod
// carries ITERION_LOG_LEVEL=info). Anything the code emits below that is not
// "quiet", it is ABSENT — which is the property under test.
func abstainingSweepFixture(t *testing.T) (*Server, string, *bytes.Buffer) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s := newForgeGateTestServer(t, st)
	logs := &bytes.Buffer{}
	s.logger = iterlog.New(iterlog.LevelWarn, logs)

	inputs := gatingInputs()
	inputs[forgePublishVarToken] = "tok-never-registered"
	const runID = "run-stuck"
	if _, err := st.CreateRun(context.Background(), runID, "review_pr", inputs); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return s, runID, logs
}

// runUpdatedAt is the instant the sweep's lookback is measured from.
func runUpdatedAt(t *testing.T, s *Server, runID string) time.Time {
	t.Helper()
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil || run == nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.UpdatedAt.IsZero() {
		t.Fatal("run carries no UpdatedAt — the sweep window cannot be measured from it")
	}
	return run.UpdatedAt
}

// The band in which that line is written has to be at least as wide as the
// interval at which the run is actually REVISITED — and since the deep pass
// resumes its cursor across passes, that interval is one full traversal of the
// horizon, not one deep pass. A backlog wider than a single pass budget makes a
// traversal several deep passes long, and a fixed two-interval band is then
// stepped clean over: the run is visited just before it and never again after.
//
// So the band is derived from the traversal the sweeper MEASURED. A guess
// pinned in a test would be a guess pinned in a contract.
func TestGateSweepAbstain_TheGiveUpBandFollowsTheMeasuredTraversal(t *testing.T) {
	// A traversal taking six deep passes — a backlog of five page budgets.
	const measured = 6
	// An age past the floor band's reach, still inside a six-pass one.
	age := gateSweepHorizon - 4*gateDeepSweepEvery*gateSweepInterval

	t.Run("a fixed floor band misses it", func(t *testing.T) {
		s, runID, logs := abstainingSweepFixture(t)
		at := runUpdatedAt(t, s, runID)
		s.gateClock = func() time.Time { return at.Add(age) }

		if err := s.reconcileGateForRunID(context.Background(), runID, gateTriggerSweep); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if logs.Len() != 0 {
			t.Fatalf("this age must be outside the floor band, or the test below proves nothing: %s", logs)
		}
	})

	t.Run("the measured band reaches it", func(t *testing.T) {
		s, runID, logs := abstainingSweepFixture(t)
		at := runUpdatedAt(t, s, runID)
		s.gateClock = func() time.Time { return at.Add(age) }
		s.noteGateDeepCycle(measured, true)

		if err := s.reconcileGateForRunID(context.Background(), runID, gateTriggerSweep); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if !strings.Contains(logs.String(), "last sweep pass") {
			t.Fatalf("a traversal of %d deep passes revisits this run every %s, so it ages out between two visits and the give-up line is never written; got: %s",
				measured, time.Duration(measured)*gateDeepSweepEvery*gateSweepInterval, logs)
		}
	})
}

// The measure must SHRINK again when the backlog does, or one busy day widens
// the band for good and the warning starts firing while the net is still
// trying — the noise the Debug/Warn split exists to prevent.
func TestGateSweepAbstain_TheBandNarrowsAgainWhenTheBacklogDoes(t *testing.T) {
	s, _, _ := abstainingSweepFixture(t)
	s.noteGateDeepCycle(6, true)
	wide := s.gateSweepLastPassMargin()
	s.noteGateDeepCycle(1, true)
	if narrow := s.gateSweepLastPassMargin(); narrow >= wide {
		t.Errorf("the band stayed at %s after a traversal that took one pass — a high-water mark never comes back down", narrow)
	}
}

// A traversal longer than the horizon means the net is not keeping up at all.
// That is one fact about the deployment, already stated once per pass by the
// page-cap warning; letting it widen the band without bound would turn it into
// a per-run Warn on every candidate in the window.
func TestGateSweepAbstain_TheBandCannotSwallowTheWholeWindow(t *testing.T) {
	s, _, _ := abstainingSweepFixture(t)
	s.noteGateDeepCycle(100000, true)
	if got := s.gateSweepLastPassMargin(); got > gateSweepHorizon/2 {
		t.Errorf("band = %s of a %s horizon — every run in the window would warn on every pass", got, gateSweepHorizon)
	}
}

// While the net is still trying, a stuck check is not news: the sweep re-offers
// the same run every minute and would otherwise emit ~60 identical lines an
// hour, per replica, burying the branches that carry new information.
func TestGateSweepAbstain_StaysQuietWhileTheNetIsStillTrying(t *testing.T) {
	s, runID, logs := abstainingSweepFixture(t)
	at := runUpdatedAt(t, s, runID)
	s.gateClock = func() time.Time { return at.Add(2 * gateSweepInterval) }

	if err := s.reconcileGateForRunID(context.Background(), runID, gateTriggerSweep); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("an early sweep pass must not log above debug, got: %s", logs)
	}
}

// The last pass is the one that matters: past the horizon the run leaves the
// candidate window and NOTHING revisits it, so whatever the reconciler
// abstained on becomes permanent. The whole sweep history below it is Debug —
// suppressed at info — so without this line a pull request blocked for 22 hours
// behind an unanswered required check leaves nothing anywhere naming the reason.
func TestGateSweepAbstain_LastPassNamesTheReasonAndThePermanence(t *testing.T) {
	s, runID, logs := abstainingSweepFixture(t)
	at := runUpdatedAt(t, s, runID)
	s.gateClock = func() time.Time { return at.Add(gateSweepHorizon - gateDeepSweepEvery*gateSweepInterval) }

	if err := s.reconcileGateForRunID(context.Background(), runID, gateTriggerSweep); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	out := logs.String()
	if out == "" {
		t.Fatal("the last sweep pass over a stuck run must be visible at warn — it is the only chance to name why the check stays unanswered")
	}
	for _, want := range []string{
		runID,                            // WHICH run owes the verdict
		"https://github.com/o/r/pull/42", // WHICH pull request is waiting
		"publish grant is expired",       // WHY it posts nothing
		"last sweep pass",                // that the miss is now permanent
		"stays unanswered",               //
	} {
		if !strings.Contains(out, want) {
			t.Errorf("last-pass warning does not carry %q, got: %s", want, out)
		}
	}
}

// The event path fires once per run and has always warned; the last-pass rule
// must not silence it. A run offered by the event before the window is old is
// exactly the ordinary case, and it is the FIRST notice an operator gets.
func TestGateAbstain_EventPathWarnsRegardlessOfWindowAge(t *testing.T) {
	s, runID, logs := abstainingSweepFixture(t)
	at := runUpdatedAt(t, s, runID)
	s.gateClock = func() time.Time { return at.Add(time.Second) }

	if err := s.reconcileGateForRunID(context.Background(), runID, gateTriggerEvent); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !strings.Contains(logs.String(), "publish grant is expired") {
		t.Fatalf("the event path must warn on the first abstain, got: %s", logs)
	}
	if strings.Contains(logs.String(), "last sweep pass") {
		t.Fatalf("the event path is not a sweep pass and must not claim to be the last one, got: %s", logs)
	}
}

// A run with no UpdatedAt cannot be placed in the window at all. Treating that
// as "the last pass" would warn on EVERY pass for such a run — the volume this
// whole rule exists to avoid.
func TestGateSweepIsLastPass_UndatableRunIsNeverTheLastPass(t *testing.T) {
	s := &Server{}
	if s.gateSweepIsLastPass(nil) {
		t.Error("a nil run must not be reported as the last pass")
	}
	if s.gateSweepIsLastPass(&store.Run{}) {
		t.Error("a run with no UpdatedAt must not be reported as the last pass")
	}
}

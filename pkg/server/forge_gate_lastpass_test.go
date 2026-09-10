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

// The last pass is the one that matters: past the lookback the run leaves the
// candidate window and NOTHING revisits it, so whatever the reconciler
// abstained on becomes permanent. Before this, the whole sweep history was
// Debug — suppressed at info — so a pull request blocked for 22 hours behind an
// unanswered required check left no line anywhere naming the reason.
func TestGateSweepAbstain_LastPassNamesTheReasonAndThePermanence(t *testing.T) {
	s, runID, logs := abstainingSweepFixture(t)
	at := runUpdatedAt(t, s, runID)
	s.gateClock = func() time.Time { return at.Add(gateSweepLookback - gateSweepInterval) }

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

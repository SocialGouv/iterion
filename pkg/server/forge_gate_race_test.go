package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// racyGateClient holds the reconciler's status READ open so a fresher run's
// verdict lands exactly in the read-then-write window — the production
// interleaving of forge_gate_reconcile.go's "read the claim, then write the
// synthetic failure", forced rather than hoped for. A test that can only
// redden in one ordering is a flaky test; this one cannot pass or fail in any
// other.
type racyGateClient struct {
	fakeGateClient
	statuses []forge.CommitStatus
	listed   chan struct{}
	release  chan struct{}
	once     chan struct{}
}

func (f *racyGateClient) ListCommitStatuses(_ context.Context, _, _ string) ([]forge.CommitStatus, error) {
	st := append([]forge.CommitStatus{}, f.statuses...)
	select {
	case <-f.once:
	default:
		close(f.once)
		close(f.listed)
		<-f.release // the verdict lands while this stale answer is held
	}
	return st, nil
}

// S2-MED: the anchor must be the run's TRUE terminal instant (FinishedAt),
// not UpdatedAt — ~20 granular setters bump UpdatedAt after the run is over
// (a budget snapshot on every resume of a failed_resumable run, …). With the
// bookkeeping timestamp as anchor, a verdict decided AFTER the real death but
// BEFORE the bump reads as "older" than the reconciler and gets painted over.
//
// Mutation that reddens this test: anchoring the reconciler's decision at
// run.UpdatedAt instead of run.FinishedAt (with the UpdatedAt fallback).
func TestGateReconcile_AnchorIsTheTerminalInstantNotBookkeeping(t *testing.T) {
	gc := &racyGateClient{
		fakeGateClient: fakeGateClient{headSHA: "deadbeef"},
		listed:         make(chan struct{}),
		release:        make(chan struct{}),
		once:           make(chan struct{}),
	}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	gc.statuses = []forge.CommitStatus{{
		State: forge.CommitStatePending, Context: "iterion/review",
		Description: gateInFlightDescription,
		TargetURL:   gateRunURL(s.cfg.PublicURL, runID),
	}}
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusCancelled
	died := time.Now().Add(-time.Hour)
	run.FinishedAt = &died
	// A granular setter stamped bookkeeping half an hour AFTER the death.
	run.UpdatedAt = died.Add(30 * time.Minute)
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- s.reconcileGateForRun(context.Background(), terminalEvent(runID)) }()
	<-gc.listed

	// The fresh run's verdict was decided 30 minutes after the TRUE death —
	// squarely inside the bookkeeping bump's shadow.
	replica2 := newForgeGateTestServer(t, s.cfg.Store)
	replica2.gateDecisions = s.gateDecisions
	replica2.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return gc, nil
	}
	conn, err := s.forgeConnections.Get(context.Background(), "conn1")
	if err != nil {
		t.Fatal(err)
	}
	gate := replica2.postGateStatus(context.Background(), conn, "o/r", 42, &publishReviewGate{
		Enabled: true, Context: "iterion/review", BlockingCount: 0, AuditedSHA: "deadbeef",
	}, "https://iterion.test/review/1", newGateDecision(died.Add(30*time.Minute)))
	if !gate.posted {
		t.Fatalf("the fresher verdict did not land: %s", gate.errText)
	}
	close(gc.release)
	if err := <-done; err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, st := range gc.posted {
		if st.State == forge.CommitStateFailure {
			t.Fatalf("anchored at the bookkeeping UpdatedAt, the reconciler painted %q over a verdict decided after the true death — FinishedAt is the anchor", st.Description)
		}
	}
}

// shared authority: the reconciler reads the pending left by dead run A, a
// fresh run B publishes success in between, and the reconciler's synthetic
// failure lands on top of a legitimate verdict. Two replicas (two Server
// instances over ONE shared decision store), a cancelled run, a late
// publication — forced in this exact order.
func TestGateReconcile_NeverOverwritesAFresherVerdict(t *testing.T) {
	gc := &racyGateClient{
		fakeGateClient: fakeGateClient{headSHA: "deadbeef"},
		listed:         make(chan struct{}),
		release:        make(chan struct{}),
		once:           make(chan struct{}),
	}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	// The pending the reconciler reads is the dead run's own in-flight claim.
	gc.statuses = []forge.CommitStatus{{
		State: forge.CommitStatePending, Context: "iterion/review",
		Description: gateInFlightDescription,
		TargetURL:   gateRunURL(s.cfg.PublicURL, runID),
	}}
	// The run was cancelled an hour ago. Its terminal instant is what anchors
	// its say on the head: a verdict decided since must ALWAYS supersede it,
	// at any wall-clock granularity.
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusCancelled
	run.UpdatedAt = time.Now().Add(-time.Hour)
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- s.reconcileGateForRun(context.Background(), terminalEvent(runID)) }()
	<-gc.listed // the reconciler now holds its stale read: pending, its own claim

	// A second replica — same shared stores, another Server — publishes the
	// fresh run's verdict through the production path, inside the window.
	replica2 := newForgeGateTestServer(t, s.cfg.Store)
	replica2.gateDecisions = s.gateDecisions // the authority both replicas share
	replica2.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return gc, nil
	}
	conn, err := s.forgeConnections.Get(context.Background(), "conn1")
	if err != nil {
		t.Fatal(err)
	}
	gate := replica2.postGateStatus(context.Background(), conn, "o/r", 42, &publishReviewGate{
		Enabled: true, Context: "iterion/review", BlockingCount: 0, AuditedSHA: "deadbeef",
	}, "https://iterion.test/review/1", newGateDecision(time.Now()))
	if !gate.posted {
		t.Fatalf("the fresher verdict did not land: %s", gate.errText)
	}
	close(gc.release)
	if err := <-done; err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The required check must still say what the fresh review decided — a
	// synthetic "review died" over it certifies a death that did not happen.
	sawSuccess := false
	for _, st := range gc.posted {
		if st.Context != "iterion/review" {
			continue
		}
		if st.State == forge.CommitStateFailure {
			t.Fatalf("the reconciler painted %q over a fresher success — a read-then-write with no shared authority (#1590)", st.Description)
		}
		if st.State == forge.CommitStateSuccess {
			sawSuccess = true
		}
	}
	if !sawSuccess {
		t.Error("the fresher verdict is not on the head at all")
	}
}

// scriptedDecisionStore stubs the verdict-order authority for the branch the
// memory store cannot reach deterministically: a refused claim whose
// incumbent is then unreadable.
type scriptedDecisionStore struct {
	claimOk bool
	m       gateMark
	found   bool
	err     error
}

func (s scriptedDecisionStore) claim(context.Context, string, gateMark, time.Duration) (bool, error) {
	return s.claimOk, nil
}
func (s scriptedDecisionStore) newest(context.Context, string) (gateMark, bool, error) {
	return s.m, s.found, s.err
}
func (s scriptedDecisionStore) release(context.Context, string, gateDecision) error { return nil }

// R3-2: two racing passes of ONE dead run anchor at the same FinishedAt, so
// the loser's claim TIES and is refused — but the tie is not "a newer verdict
// owns the check": it is this run's own twin (or a same-ms coincidence), and
// the winner's post can still fail. Settling superseded there — a PERMANENT
// mark — strands the required check when it does. On a tie the pass must
// leave the run offered: the next sweep either finds the twin's posted
// synthetic (cheap speaksFor exit) or re-claims after its release and posts
// the repair itself.
//
// Mutation that reddens this test: settling gateSettledSuperseded without
// requiring a strictly-newer incumbent.
func TestGateReconcile_TiedClaimDoesNotSettleAndTheRepairStillLands(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	died := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusCancelled
	run.FinishedAt = &died
	run.UpdatedAt = died
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}

	// The twin pass claimed first, at the SAME truncated anchor.
	conn, err := s.forgeConnections.Get(context.Background(), "conn1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	markKey := gateDecisionKey(conn, "o/r", "deadbeef", "iterion/review")
	twin := gateDecisionAt(died)
	claimed, err := s.claimGateDecision(ctx, markKey, gateMark{Decision: twin, Status: gateMarkStatus{
		State: string(forge.CommitStateFailure), Description: gateInterruptedDescription,
	}})
	if err != nil || !claimed {
		t.Fatalf("twin claim: granted=%v err=%v", claimed, err)
	}

	// The losing pass: refused on the tie, must NOT settle.
	if err := s.reconcileGateForRun(ctx, terminalEvent(runID)); err != nil {
		t.Fatalf("reconcile (tie): %v", err)
	}
	marks, _ := s.gateSettles.settled(ctx, []string{runID})
	if m, ok := marks[runID]; ok {
		t.Fatalf("a tied claim settled the run %q — had the twin's post failed, the check would now strand forever", m.Reason)
	}

	// The twin's post fails and its claim is released: nothing was posted,
	// the head is still bare, and the run is still offered.
	s.releaseGateDecision(ctx, markKey, twin)
	if err := s.reconcileGateForRun(ctx, terminalEvent(runID)); err != nil {
		t.Fatalf("reconcile (after release): %v", err)
	}
	if gc.setCalls != 1 {
		t.Fatalf("the repair did not land after the winner released (%d writes, want 1) — the permanent superseded settle was the only thing it needed to escape", gc.setCalls)
	}
}

// R4-3/R5-2: the tie/miss branch mirrors the abstain path's levels — Warn on
// the event path, Debug on a mid-horizon sweep pass — and a run it leaves
// offered ages out of the sweep horizon like any other, so the LAST offer
// says so at Warn, or a check that never gets answered strands in silence.
func TestGateReconcile_TiedClaimWarnsOnTheLastOffer(t *testing.T) {
	out := &lockedBuffer{}
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	s.logger = iterlog.New(iterlog.LevelWarn, out)
	died := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusCancelled
	run.FinishedAt = &died
	run.UpdatedAt = time.Now()
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	conn, err := s.forgeConnections.Get(context.Background(), "conn1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	markKey := gateDecisionKey(conn, "o/r", "deadbeef", "iterion/review")
	twin := gateDecisionAt(died)
	if claimed, err := s.claimGateDecision(ctx, markKey, gateMark{Decision: twin, Status: gateMarkStatus{
		State: string(forge.CommitStateFailure), Description: gateInterruptedDescription,
	}}); err != nil || !claimed {
		t.Fatalf("twin claim: granted=%v err=%v", claimed, err)
	}

	// The event path warns once per run — the abstain rule.
	if err := s.reconcileGateForRunID(ctx, runID, gateTriggerEvent); err != nil {
		t.Fatalf("reconcile (event): %v", err)
	}
	if !strings.Contains(out.String(), runID) || strings.Contains(out.String(), "last sweep offer") {
		t.Fatalf("the event-path offer must warn naming the run (no horizon talk), got %q", out.String())
	}

	// A mid-horizon sweep pass is Debug: nothing more reaches a Warn logger.
	before := len(out.String())
	if err := s.reconcileGateForRunID(ctx, runID, gateTriggerSweep); err != nil {
		t.Fatalf("reconcile (mid-horizon sweep): %v", err)
	}
	if len(out.String()) != before {
		t.Fatalf("a mid-horizon sweep pass must stay at Debug (~60 Info/hour for a stuck tie), got %q", out.String()[before:])
	}

	// The last sweep offer, run aging out still unanswered: it must speak.
	run.UpdatedAt = time.Now().Add(-gateSweepHorizon)
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileGateForRunID(ctx, runID, gateTriggerSweep); err != nil {
		t.Fatalf("reconcile (last sweep): %v", err)
	}
	if !strings.Contains(out.String(), "last sweep offer") || !strings.Contains(out.String(), runID) {
		t.Fatalf("the last offer of a run still unanswered must warn naming the run, got %q", out.String())
	}
}

// The premise the superseded settle exists for still holds when the
// incumbent is STRICTLY newer: a real decision claimed after this run died —
// its verdict is posted, or claimed and about to be, and re-offering the run
// would only race it.
func TestGateReconcile_StrictlyNewerIncumbentStillSettlesSuperseded(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	died := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusCancelled
	run.FinishedAt = &died
	run.UpdatedAt = died
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	conn, err := s.forgeConnections.Get(context.Background(), "conn1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	markKey := gateDecisionKey(conn, "o/r", "deadbeef", "iterion/review")
	newer := gateDecisionAt(died.Add(time.Minute))
	claimed, err := s.claimGateDecision(ctx, markKey, gateMark{Decision: newer, Status: gateMarkStatus{
		State: string(forge.CommitStateSuccess), Description: "no blocking findings (≥high); 0 total",
	}})
	if err != nil || !claimed {
		t.Fatalf("newer claim: granted=%v err=%v", claimed, err)
	}

	if err := s.reconcileGateForRun(ctx, terminalEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gc.setCalls != 0 {
		t.Fatalf("painted over a strictly newer incumbent (%d writes)", gc.setCalls)
	}
	marks, _ := s.gateSettles.settled(ctx, []string{runID})
	if m := marks[runID]; m.Reason != gateSettledSuperseded {
		t.Fatalf("a strictly newer incumbent must settle superseded, got %+v", m)
	}
}

// A refused claim whose incumbent cannot be read settles nothing either: the
// settle's premise ("the incumbent posted, or will") is unverifiable, so the
// run stays offered — the cheap exit or the repair is decided by a pass that
// CAN read.
func TestGateReconcile_UnreadableIncumbentSettlesNothing(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	died := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusCancelled
	run.FinishedAt = &died
	run.UpdatedAt = died
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	s.gateDecisions = scriptedDecisionStore{claimOk: false, found: false}

	if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gc.setCalls != 0 {
		t.Fatalf("wrote without the authority's answer (%d writes)", gc.setCalls)
	}
	marks, _ := s.gateSettles.settled(context.Background(), []string{runID})
	if m, ok := marks[runID]; ok {
		t.Fatalf("an unreadable incumbent settled the run %q — an unverifiable premise parked the repair", m.Reason)
	}
}

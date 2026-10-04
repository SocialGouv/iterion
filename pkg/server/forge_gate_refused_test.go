package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// #1632 — a gate verdict REFUSED for a stale audited_sha writes nothing: not
// success, not failure, not pending. On a repo that requires the check the PR
// then has no path to merge, and nothing else fills the gap: the reconciler
// stands down on the same head-moved test ("leaving the newer head to its own
// review") but review_on_sync is OFF by default, so no newer-head review
// exists. The run finishes CLEANLY — it is never dead — so no lane consumes
// its gate_posted=false.

// publishRefusedGate drives the production publish path into the stale-pin
// refusal: the bot audited deadbeef, the head has since moved to cafebabe.
func publishRefusedGate(t *testing.T, s *Server, auditedSHA string) publishReviewResponse {
	t.Helper()
	body := `{"pr_url":"https://github.com/o/r/pull/42","summary":"reviewed",` +
		`"gate":{"enabled":true,"context":"iterion/review","blocking_count":0,"audited_sha":"` + auditedSHA + `"}}`
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok-gate", body))
	if w.Code != http.StatusOK {
		t.Fatalf("publish: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp publishReviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.GatePosted {
		t.Fatalf("the stale pin must be REFUSED, not posted: %+v", resp)
	}
	return resp
}

func finishGateRun(t *testing.T, s *Server, runID string) {
	t.Helper()
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusFinished
	run.UpdatedAt = time.Now().Add(-time.Minute)
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
}

func finishedEvent(runID string) trigger.Event {
	return trigger.Event{Source: trigger.SourceRun, Kind: trigger.KindRunFinished, Subject: trigger.Subject{ID: runID}}
}

// The gap itself: refused for a stale pin, head moved, nothing on the new
// head — the reconciler must answer the CURRENT head, with a diagnosis that
// says REFUSED (the review completed; the gate declined it), never "review
// died" (it did not) and never silence (the PR waits forever).
func TestGateReconcile_RefusedStalePinAnswersTheNewHead(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "cafebabe"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	finishGateRun(t, s, runID)

	resp := publishRefusedGate(t, s, "deadbeef")
	if !strings.Contains(resp.GateError, "head moved") {
		t.Fatalf("expected the stale-pin refusal, got gate_error=%q", resp.GateError)
	}

	if err := s.reconcileGateForRun(context.Background(), finishedEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gc.setCalls != 1 {
		t.Fatalf("posted %d statuses, want 1 — the refused run answered nothing, and an absent required check blocks the PR forever (#1632)", gc.setCalls)
	}
	if gc.lastSHA != "cafebabe" {
		t.Errorf("posted on %q, want the CURRENT head cafebabe — that is the revision the merge decision waits on", gc.lastSHA)
	}
	if gc.last.State != forge.CommitStateFailure {
		t.Errorf("state = %q, want failure — a refused review has approved nothing", gc.last.State)
	}
	if !strings.HasPrefix(gc.last.Description, "review refused to certify") {
		t.Errorf("description = %q — the diagnosis is REFUSED, not \"review died\": the remedy differs (the review completed; re-review the new head)", gc.last.Description)
	}
	if strings.Contains(gc.last.Description, "deadbeef") != true {
		t.Errorf("description must name the audited revision: %q", gc.last.Description)
	}
}

// The cost design (a) named: the refusing run must NEVER paint over a
// legitimate verdict a fresher run posted on the new head. Read live, never
// overwritten.
func TestGateReconcile_RefusedStalePinNeverOverwritesANewerVerdict(t *testing.T) {
	gc := &listingGateClient{
		fakeGateClient: fakeGateClient{headSHA: "cafebabe"},
		statuses: []forge.CommitStatus{{
			Context: "iterion/review", State: forge.CommitStateSuccess,
			Description: "no blocking findings (≥high); 0 total",
		}},
	}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	finishGateRun(t, s, runID)
	publishRefusedGate(t, s, "deadbeef")

	if err := s.reconcileGateForRun(context.Background(), finishedEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gc.setCalls != 0 {
		t.Fatalf("overwrote a fresher run's verdict to fill the gap (%d writes) — the one outcome worse than the gap itself", gc.setCalls)
	}
}

// review_on_sync's fresh review, or any live run, claims the new head with
// its own pending: the answer is coming, and the refusal must not paint
// "refused" over a live review — design (b)'s "stand down when on", read off
// the forge rather than off a config the publish path does not hold.
func TestGateReconcile_RefusedStalePinStandsDownOnAFreshReviewInFlight(t *testing.T) {
	gc := &listingGateClient{
		fakeGateClient: fakeGateClient{headSHA: "cafebabe"},
		statuses: []forge.CommitStatus{{
			Context: "iterion/review", State: forge.CommitStatePending,
			Description: gateInFlightDescription,
			TargetURL:   "https://iterion.test/studio/runs/run-fresher",
		}},
	}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	finishGateRun(t, s, runID)
	publishRefusedGate(t, s, "deadbeef")

	if err := s.reconcileGateForRun(context.Background(), finishedEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gc.setCalls != 0 {
		t.Fatalf("painted over a fresh review's in-flight claim (%d writes)", gc.setCalls)
	}
}

// R2-S3-LOW2: the clear is a compare-and-swap, like replaceDeferral — a
// refusal recorded AFTER the one a pass consumed (a fresher episode, inside
// the post→clear window) is not that pass's to retire. Clearing
// unconditionally degrades "honored at most once" into "possibly zero".
//
// Mutation that reddens this test: clearGateRefusal nil-ing grant.Refusal
// without the identity match.
func TestClearGateRefusal_NeverSwallowsAFresherRefusal(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok-gate", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	r1 := &gateRefusal{AuditedSHA: "deadbeef", HeadSHA: "cafebabe", Context: "iterion/review", Reason: "r1", At: time.Now().Add(-time.Hour)}
	r2 := &gateRefusal{AuditedSHA: "deadbeef", HeadSHA: "0ther5ha", Context: "iterion/review", Reason: "r2", At: time.Now()}
	// The grant carries R2 — recorded mid-pass, after this pass consumed R1.
	if _, err := s.forgePublishTokens.update("tok-gate", func(g *ForgePublishGrant) { g.Refusal = r2 }); err != nil {
		t.Fatal(err)
	}

	s.clearGateRefusal("tok-gate", r1)
	if grant, ok := s.forgePublishTokens.lookup("tok-gate"); !ok || grant.Refusal != r2 {
		t.Fatalf("clearing the CONSUMED refusal swallowed the fresher one — its episode is left with no diagnosis to honor")
	}
	s.clearGateRefusal("tok-gate", r2)
	if grant, ok := s.forgePublishTokens.lookup("tok-gate"); ok && grant.Refusal != nil {
		t.Fatalf("the refusal that WAS consumed survived its clear: %+v", grant.Refusal)
	}
}

// S3-LOW1: boundedRunes bounds RUNES (a GitHub description limit counts
// characters), ellipsis included, and never panics on a non-positive budget.
func TestBoundedRunes(t *testing.T) {
	if got := boundedRunes(strings.Repeat("é", 40), 60); got != strings.Repeat("é", 40) {
		t.Errorf("shorter than the budget must pass through, got %d runes", len([]rune(got)))
	}
	if got := boundedRunes(strings.Repeat("é", 80), 60); len([]rune(got)) != 60 || !strings.HasSuffix(got, "…") {
		t.Errorf("80 runes over a 60 budget: got %d runes", len([]rune(got)))
	}
	if got := boundedRunes("anything", 0); got != "" {
		t.Errorf("n=0 must yield empty, got %q", got)
	}
}

// S3-HIGH2: the refusal settlement must be REVERSIBLE like head_moved (the
// settlement the refusal replaced): a force-push back to the refused head
// makes the repair reachable again, and a permanent mark would never
// re-offer the run.
func TestGateSettle_RefusedSettlementIsReversible(t *testing.T) {
	if !gateSettleReversible(gateSettledRefused) {
		t.Fatal("a force-push back to the refused head is never repaired: the sweep never re-offers a permanently-settled run")
	}
	for _, reason := range []string{gateSettledClosed, gateSettledHeadMoved} {
		if !gateSettleReversible(reason) {
			t.Errorf("%s lost its reversibility", reason)
		}
	}
	if gateSettleReversible(gateSettledVerdictSuccess) {
		t.Error("a real verdict stays permanent — nothing can change it back")
	}
}

// S3-MED1: a DEFERRED verdict whose replay meets the stale-pin refusal is

// recorded on the STORE copy of the grant — but the reconciler keeps the
// pre-replay in-memory copy, so without the re-read the pass that recorded
// the refusal is blind to it: nothing is answered, and the run parks on a
// reversible head-moved settlement for up to six hours with the answer in
// hand.
//
// Mutation that reddens this test: reconciling from the pre-replay grant copy
// (no re-lookup after replayGateDeferral returns false).
func TestGateReconcile_DeferredReplayRefusalIsAnsweredInTheSamePass(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "cafebabe"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	finishGateRun(t, s, runID)

	// The grant carries a PINNED deferred verdict whose wait is over; the
	// head has since moved, so the replay is refused for the stale pin.
	if _, err := s.forgePublishTokens.update("tok-gate", func(g *ForgePublishGrant) {
		g.Deferred = &gateDeferral{
			Gate:     publishReviewGate{Enabled: true, Context: "iterion/review", BlockingCount: 0, AuditedSHA: "deadbeef"},
			Repo:     "o/r",
			Number:   42,
			Decision: newGateDecision(time.Now().Add(-time.Hour)),
			RetryAt:  time.Now().Add(-time.Minute),
			Attempts: 1,
		}
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.reconcileGateForRun(context.Background(), finishedEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gc.setCalls != 1 {
		t.Fatalf("posted %d statuses, want 1 — the replay's refusal was recorded and then ignored by the very pass that recorded it", gc.setCalls)
	}
	if gc.lastSHA != "cafebabe" {
		t.Errorf("posted on %q, want the current head", gc.lastSHA)
	}
	if !strings.HasPrefix(gc.last.Description, "review refused to certify") {
		t.Errorf("description = %q, want the refusal diagnosis", gc.last.Description)
	}
}

// S3-MED2: a refusal is episode-scoped state on an episode-blind store. The
// endpoint that records it knows no run (the token is the authority), so it
// cannot be stamped with the episode — what keeps a LATER dead episode of the
// same run from inheriting the diagnosis is that the reconciler retires the
// record when it consumes or settles the episode, and a posted verdict
// retires it too.
//
// Mutation that reddens this test: not clearing grant.Refusal on consumption
// (episode 2 is answered with episode 1's refusal).
func TestGateReconcile_ALaterEpisodeInheritsNoStaleRefusal(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "cafebabe"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	finishGateRun(t, s, runID)
	publishRefusedGate(t, s, "deadbeef")

	// Episode 1: the refusal is answered and retired.
	if err := s.reconcileGateForRun(context.Background(), finishedEvent(runID)); err != nil {
		t.Fatalf("reconcile ep1: %v", err)
	}
	if gc.setCalls != 1 {
		t.Fatalf("episode 1 was not answered (calls=%d)", gc.setCalls)
	}
	if grant, ok := s.forgePublishTokens.lookup("tok-gate"); ok && grant.Refusal != nil {
		t.Fatalf("the refusal survived its consumption — episode 2 will inherit a diagnosis it never earned")
	}

	// Episode 2: resumed, died again WITHOUT publishing, head still moved.
	// Nothing may paint episode 1's refusal here — the run never tried to
	// certify anything this episode.
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusCancelled
	run.UpdatedAt = time.Now()
	fin := time.Now()
	run.FinishedAt = &fin
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
		t.Fatalf("reconcile ep2: %v", err)
	}
	for _, st := range gc.posted[1:] {
		if strings.HasPrefix(st.Description, "review refused to certify") {
			t.Fatalf("episode 2 was answered with episode 1's refusal — the run never tried to certify anything this episode")
		}
	}
}

// A verdict posted after a refusal retires the refusal: the run DID certify
// in the end, and no later reconcile may diagnose a refusal that is moot.
func TestGateReconcile_PostedVerdictRetiresTheRefusal(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, _ := gateReconcileFixture(t, gatingInputs(), gc)
	// The refusal: audited garbage while the head is deadbeef (unreadable pin).
	publishRefusedGate(t, s, "{{ not-a-sha }}")
	if grant, ok := s.forgePublishTokens.lookup("tok-gate"); !ok || grant.Refusal == nil {
		t.Fatal("precondition: the refusal is recorded")
	}

	// A later publish of the same run lands its verdict.
	resp := func() publishReviewResponse {
		body := `{"pr_url":"https://github.com/o/r/pull/42","summary":"reviewed",` +
			`"gate":{"enabled":true,"context":"iterion/review","blocking_count":0,"audited_sha":"deadbeef"}}`
		w := httptest.NewRecorder()
		s.handleForgePublishReview(w, publishReq("tok-gate", body))
		if w.Code != http.StatusOK {
			t.Fatalf("publish: code=%d body=%s", w.Code, w.Body.String())
		}
		var r publishReviewResponse
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}()
	if !resp.GatePosted {
		t.Fatalf("the verdict did not land: %s", resp.GateError)
	}
	if grant, ok := s.forgePublishTokens.lookup("tok-gate"); ok && grant.Refusal != nil {
		t.Fatalf("a posted verdict must retire the earlier refusal — it is moot, and a later dead episode would inherit it")
	}
}

// R2-S3-LOW1: GitHub truncates a status description at 140 runes. The
// refusal envelope (prefix + remedy) is 83 runes, so the reason must give
// way — a longer one pushes the REMEDY (the part the operator cannot
// reconstruct) off the end.
func TestGateRefusalDescription_StaysUnderTheForgeCap(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	finishGateRun(t, s, runID)
	// A long, UNREADABLE pin: the reason quotes it (bounded at 24 runes at
	// record time), which is exactly the shape that used to reach 143 runes.
	// (All-hex would be the stale-pin refusal — a short reason.)
	publishRefusedGate(t, s, "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	if err := s.reconcileGateForRun(context.Background(), finishedEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gc.setCalls != 1 {
		t.Fatalf("posted %d statuses, want 1", gc.setCalls)
	}
	if got := len([]rune(gc.last.Description)); got > 140 {
		t.Errorf("description is %d runes — GitHub truncates at 140, eating the remedy: %q", got, gc.last.Description)
	}
	if !strings.HasSuffix(gc.last.Description, "re-review") {
		t.Errorf("the remedy tail must survive intact: %q", gc.last.Description)
	}
}

// The wrong-diagnosis family: the review did NOT die — it was refused — but
// with the head unchanged (or moved back) the reconciler's synthetic failure
// would read "review died — push again". The refusal is on the grant; say so.
func TestGateReconcile_RefusedPinNamesTheRefusalNotADeath(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	finishGateRun(t, s, runID)

	// An unreadable pin: the bot rendered a template instead of a commit id.
	// The head never moved — reviewed == head — so only the diagnosis is new.
	resp := publishRefusedGate(t, s, "{{ not-a-sha }}")
	if !strings.Contains(resp.GateError, "not a commit id") {
		t.Fatalf("expected the unreadable-pin refusal, got gate_error=%q", resp.GateError)
	}

	if err := s.reconcileGateForRun(context.Background(), finishedEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gc.setCalls != 1 {
		t.Fatalf("posted %d statuses, want 1 — the check is absent and the run is over", gc.setCalls)
	}
	if !strings.HasPrefix(gc.last.Description, "review refused to certify") {
		t.Errorf("description = %q — says \"died\" of a review that was REFUSED; the operator re-runs a bot whose pin template is broken and gets the same refusal", gc.last.Description)
	}
}

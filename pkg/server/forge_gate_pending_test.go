package server

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
)

// inFlightVars is a review-shaped launch: the repo pinned a gate context and
// the lane resolved the revision to review.
func inFlightVars() map[string]string {
	return map[string]string{
		"pr_url":       "https://github.com/o/r/pull/42",
		"gate_context": "iterion/review",
		"head_sha":     "deadbeef",
	}
}

func inFlightFixture(t *testing.T, gc forgeGateClient) *Server {
	t.Helper()
	s, _ := newForgePublishTestServer(t)
	s.cfg.PublicURL = "https://iterion.test"
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return gc, nil
	}
	return s
}

// The window between the push and the verdict is minutes long. With no status
// on the head the forge renders the required check as "Expected — waiting for
// status to be reported", which is byte-identical to a review that was never
// launched at all. Claiming the context at launch is what separates the two.
func TestMarkGateInFlight_ClaimsAnAbsentCheck(t *testing.T) {
	gc := &listingGateClient{}
	s := inFlightFixture(t, gc)

	s.markGateInFlight(context.Background(), "team1", "review-pr", inFlightVars(), "run-42")

	if gc.setCalls != 1 {
		t.Fatalf("posted %d statuses, want 1 — the check stays absent for the whole review", gc.setCalls)
	}
	if gc.last.State != forge.CommitStatePending {
		t.Errorf("state = %q, want pending — the review has not answered yet", gc.last.State)
	}
	if gc.last.Context != "iterion/review" {
		t.Errorf("context = %q, want the gate the repo pinned", gc.last.Context)
	}
	if !isGateInFlight(gc.last) {
		t.Errorf("status %q is not recognisable as the in-flight marker — the reconciler would read it as a verdict and stand down", gc.last.Description)
	}
	if gc.last.TargetURL != "https://iterion.test/runs/run-42" {
		t.Errorf("target url = %q, want the live run console", gc.last.TargetURL)
	}
	if gc.lastSHA != "deadbeef" {
		t.Errorf("posted on %q, want the revision the run was handed", gc.lastSHA)
	}
}

// A repo pins ONE gate context precisely so a required check can span several
// bots. A second bot launching on a head another bot has already judged must
// not blank that judgment back to "running" — the verdict would be lost and
// the merge gate would reopen on a PR that was legitimately red.
func TestMarkGateInFlight_NeverOverwritesAVerdict(t *testing.T) {
	for _, verdict := range []forge.CommitState{forge.CommitStateSuccess, forge.CommitStateFailure} {
		gc := &listingGateClient{statuses: []forge.CommitStatus{
			{Context: "iterion/review", State: verdict, Description: "3 blocking finding(s) ≥high"},
		}}
		s := inFlightFixture(t, gc)

		s.markGateInFlight(context.Background(), "team1", "review-pr", inFlightVars(), "run-42")

		if gc.setCalls != 0 {
			t.Errorf("%s verdict: posted %d statuses, want 0 — a bot's judgment was blanked to pending", verdict, gc.setCalls)
		}
	}
}

// The synthetic failure means "a review died here"; a fresh review starting on
// that same head is exactly the recovery, so the claim replaces it. Same for a
// stale claim of its own: re-reviewing a head must re-point the check at the
// run that is actually working.
func TestMarkGateInFlight_ReplacesAnInterruptionAndItsOwnStaleClaim(t *testing.T) {
	cases := map[string]forge.CommitStatus{
		"synthetic interruption": {Context: "iterion/review", State: forge.CommitStateFailure, Description: gateInterruptedDescription},
		"its own stale claim":    {Context: "iterion/review", State: forge.CommitStatePending, Description: gateInFlightDescription},
	}
	for name, existing := range cases {
		t.Run(name, func(t *testing.T) {
			gc := &listingGateClient{statuses: []forge.CommitStatus{existing}}
			s := inFlightFixture(t, gc)

			s.markGateInFlight(context.Background(), "team1", "review-pr", inFlightVars(), "run-99")

			if gc.setCalls != 1 {
				t.Fatalf("posted %d statuses, want 1 — a live review must claim the check", gc.setCalls)
			}
			if !isGateInFlight(gc.last) {
				t.Errorf("status = %s/%q, want the in-flight marker", gc.last.State, gc.last.Description)
			}
			if gc.last.TargetURL != "https://iterion.test/runs/run-99" {
				t.Errorf("target url = %q, want the run that is actually working", gc.last.TargetURL)
			}
		})
	}
}

// A launch that gates nothing must cost no forge traffic: most runs carry no
// gate_context, and a claim on a head we were not given is a status on the
// wrong commit.
func TestMarkGateInFlight_NoOpsWithoutAGateOrARevision(t *testing.T) {
	cases := map[string]func(map[string]string){
		"no gate context": func(v map[string]string) { delete(v, "gate_context") },
		"no head sha":     func(v map[string]string) { delete(v, "head_sha") },
		"no pr url":       func(v map[string]string) { delete(v, "pr_url") },
	}
	for name, strip := range cases {
		t.Run(name, func(t *testing.T) {
			gc := &listingGateClient{}
			s := inFlightFixture(t, gc)
			vars := inFlightVars()
			strip(vars)

			s.markGateInFlight(context.Background(), "team1", "review-pr", vars, "run-42")

			if gc.setCalls != 0 || gc.listCalls != 0 {
				t.Errorf("%d posts / %d reads, want none — a launch that gates nothing must not touch the forge", gc.setCalls, gc.listCalls)
			}
		})
	}
}

// A claim is not an answer, but WHOSE claim it is decides everything.
//
// Own claim: the regression the marker creates if it is not accounted for. The
// reconciler stands down on any status it did not recognise as unanswered, so
// a run that died still holding its claim would look "already answered" and
// the PR would wait on a pending nothing resolves — strictly worse than the
// absent check the marker was introduced to fix.
//
// Another run's claim: the mirror hazard, and the one that actually shipped
// broken. The repair's own recovery relaunches a bot, whose launch claims the
// head; the sweep then re-offers the DEAD run minutes later and would paint
// "review died" over a review that is running right now — and re-enter a
// relaunch whose idempotency key is spent, filing a board card telling a human
// the automation is out of moves while the replacement is alive and working.
// The same clobber happens whenever two bots share the repo's one gate
// context, which is the documented reason a repo pins one.
func TestGateReconcile_ActsOnItsOwnClaimAndLeavesAnotherRunsAlone(t *testing.T) {
	claim := func(targetURL string) forge.CommitStatus {
		return forge.CommitStatus{
			Context:     "iterion/review",
			State:       forge.CommitStatePending,
			Description: gateInFlightDescription,
			TargetURL:   targetURL,
		}
	}
	t.Run("its own claim — nobody else will answer it", func(t *testing.T) {
		gc := &listingGateClient{
			fakeGateClient: fakeGateClient{headSHA: "deadbeef"},
			statuses:       []forge.CommitStatus{claim("https://iterion.test/runs/run-gating")},
		}
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)

		if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if gc.setCalls != 1 {
			t.Fatalf("posted %d statuses, want 1 — the PR is left waiting on a pending nothing will resolve", gc.setCalls)
		}
		if gc.last.State != forge.CommitStateFailure {
			t.Errorf("state = %q, want failure — the run died without answering its own claim", gc.last.State)
		}
	})
	t.Run("another run's claim — a live review must not be blamed", func(t *testing.T) {
		gc := &listingGateClient{
			fakeGateClient: fakeGateClient{headSHA: "deadbeef"},
			statuses:       []forge.CommitStatus{claim("https://iterion.test/runs/run-the-replacement")},
		}
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)

		if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if gc.setCalls != 0 {
			t.Fatalf("posted %d statuses, want 0 — \"review died\" was painted over a review that is running, and the recovery re-entered", gc.setCalls)
		}
	})
}

// With no PublicURL configured a status cannot name the run it speaks for, so
// "mine" and "another run's" become the same observation. The launch therefore
// does not claim at all (the check behaves as it did before the feature), and
// the repair stands down on an existing synthetic failure rather than re-post
// and re-enter the recovery on every one of the sweep's ~57 passes per hour.
func TestGateWithoutPublicURL_ClaimsNothingAndDoesNotLoop(t *testing.T) {
	t.Run("the launch posts no unattributable claim", func(t *testing.T) {
		gc := &listingGateClient{}
		s := inFlightFixture(t, gc)
		s.cfg.PublicURL = ""

		s.markGateInFlight(context.Background(), "team1", "review-pr", inFlightVars(), "run-42")

		if gc.setCalls != 0 {
			t.Fatalf("posted %d statuses, want 0 — an unattributable claim can never be told from another run's", gc.setCalls)
		}
	})
	t.Run("the repair does not re-post over an existing synthetic failure", func(t *testing.T) {
		gc := &listingGateClient{
			fakeGateClient: fakeGateClient{headSHA: "deadbeef"},
			statuses: []forge.CommitStatus{
				{Context: "iterion/review", State: forge.CommitStateFailure, Description: gateInterruptedDescription},
			},
		}
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)
		s.cfg.PublicURL = ""

		if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if gc.setCalls != 0 {
			t.Fatalf("posted %d statuses, want 0 — every sweep pass would re-post and re-enter the recovery", gc.setCalls)
		}
	})
}

// A run parked on a provider usage window is resumed by the retry sweeper up
// to retrypolicy.DefaultMaxWait later — a weekly forfait cap resets as much as
// seven days out. If the publish grant dies first, the resumed review runs to
// completion and then cannot post the verdict it computed: the required check
// keeps whatever the interruption left, and the PR waits on an answer that
// exists. The grant must outlive the longest wait the retry machinery can
// schedule.
func TestForgePublishGrantOutlivesTheLongestRetryWait(t *testing.T) {
	if forgePublishDefaultTTL <= retrypolicy.DefaultMaxWait {
		t.Fatalf("publish grant TTL %s <= max retry wait %s — a review resumed after a long quota window cannot post its verdict",
			forgePublishDefaultTTL, retrypolicy.DefaultMaxWait)
	}
	// And with room for the resumed run itself, not merely one second more.
	if margin := forgePublishDefaultTTL - retrypolicy.DefaultMaxWait; margin < 6*time.Hour {
		t.Errorf("margin over the max retry wait is %s — too tight for the resumed run to finish and publish", margin)
	}
}

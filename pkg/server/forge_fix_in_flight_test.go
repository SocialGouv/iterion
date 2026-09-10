package server

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// fixLaunchFixture is inFlightFixture plus the REAL bots/ catalog, because the
// role is derived from each bot's manifest produces:/consumes: and the engine
// names no bot. Without the catalog every role reads `unknown` and every
// assertion below would pass vacuously — which is exactly what the first draft
// of this file did.
func fixLaunchFixture(t *testing.T, gc forgeGateClient) *Server {
	t.Helper()
	s := inFlightFixture(t, gc)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	return s
}

// fixLaunchVars is a fixer-shaped launch: a pull request and the revision it
// is about to rewrite. Deliberately carries a gate_context too — a fixer
// launched through a repo that pins one must still not write there.
func fixLaunchVars() map[string]string {
	return map[string]string{
		"pr_url":       "https://github.com/o/r/pull/42",
		"head_sha":     "deadbeef",
		"gate_context": "iterion/review",
	}
}

// The window this closes: a fixer works for tens of minutes and holds no
// required check, so until it reported, NOTHING on the pull request said it
// was there. A second writer pushing in that window collides with the
// push-back, the auto-rebase conflicts, and the pass is banked instead of
// landed. Measured twice on one PR on 2026-09-09.
func TestMarkFixInFlight_ClaimsForAFixer(t *testing.T) {
	gc := &listingGateClient{}
	s := fixLaunchFixture(t, gc)

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")

	if gc.setCalls != 1 {
		t.Fatalf("posted %d statuses, want 1 — the fixer stays invisible for its whole run", gc.setCalls)
	}
	if gc.last.State != forge.CommitStatePending {
		t.Errorf("state = %q, want pending — the fixer has not reported yet", gc.last.State)
	}
	if !isFixInFlight(gc.last) {
		t.Errorf("status %q is not recognisable as the fixer claim — the terminal clear would leave it pending forever", gc.last.Description)
	}
	if gc.last.TargetURL != "https://iterion.test/runs/run-77" {
		t.Errorf("target url = %q, want the live run console — an unattributable claim can never be resolved", gc.last.TargetURL)
	}
	if gc.lastSHA != "deadbeef" {
		t.Errorf("posted on %q, want the revision the run was handed", gc.lastSHA)
	}
}

// THE load-bearing separation. The fixer must never occupy the context branch
// protection may require: writing there could blank a reviewer's verdict back
// to "running" — the exact harm markGateInFlight is written to avoid — and
// could block a merge on a signal that is meant to be advisory.
func TestMarkFixInFlight_NeverWritesOnTheGateContext(t *testing.T) {
	gc := &listingGateClient{}
	s := fixLaunchFixture(t, gc)
	vars := fixLaunchVars()

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", vars, "run-77")

	if gc.last.Context == vars["gate_context"] {
		t.Fatalf("the fixer claimed %q, the repo's REQUIRED gate — a reviewer verdict there would be blanked to running", gc.last.Context)
	}
	if gc.last.Context != fixInFlightContext {
		t.Errorf("context = %q, want %q", gc.last.Context, fixInFlightContext)
	}
}

// The role decides, and it comes from the manifest — never a bot id. A
// REVIEWER already claims the gate context; giving it a second claim would put
// a pending status on every reviewed head that nothing in the reviewer lane
// ever resolves.
func TestMarkFixInFlight_SilentForANonFixer(t *testing.T) {
	for _, bot := range []string{"review-pr", "", "a-bot-no-catalog-knows"} {
		gc := &listingGateClient{}
		s := fixLaunchFixture(t, gc)

		s.markFixInFlight(context.Background(), "team1", "", bot, fixLaunchVars(), "run-77")

		if gc.setCalls != 0 {
			t.Errorf("bot %q claimed the fixer context (%d posts) — only a bot that CONSUMES a review is a fixer", bot, gc.setCalls)
		}
	}
}

// A launch that names no pull request, or no revision, has nothing to claim
// on. Silence, not a guess.
func TestMarkFixInFlight_NeedsAPRAndARevision(t *testing.T) {
	for name, mutate := range map[string]func(map[string]string){
		"no pr_url":   func(v map[string]string) { delete(v, "pr_url") },
		"no head_sha": func(v map[string]string) { delete(v, "head_sha") },
	} {
		gc := &listingGateClient{}
		s := fixLaunchFixture(t, gc)
		vars := fixLaunchVars()
		mutate(vars)

		s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", vars, "run-77")

		if gc.setCalls != 0 {
			t.Errorf("%s: posted anyway (%d)", name, gc.setCalls)
		}
	}
}

// Read before write. A status somebody else owns on this context is not ours
// to blank — the same discipline the gate claim keeps.
func TestMarkFixInFlight_NeverOverwritesAForeignStatus(t *testing.T) {
	for _, st := range []forge.CommitState{forge.CommitStateSuccess, forge.CommitStateFailure, forge.CommitStatePending} {
		gc := &listingGateClient{statuses: []forge.CommitStatus{
			{Context: fixInFlightContext, State: st, Description: "something another tool posted"},
		}}
		s := fixLaunchFixture(t, gc)

		s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")

		if gc.setCalls != 0 {
			t.Errorf("state %q: overwrote a status this server does not own", st)
		}
	}
}

// A claim that is never resolved is worse than none: it would tell every later
// reader that a run is working when none is. The terminal clear RESOLVES it
// rather than deleting it — a status that vanishes reads as "never claimed",
// R51e567 — IsTerminal() includes failed_resumable, and that is exactly where
// a fixer lands when it parks on a usage window: with a durable retry the
// sweeper will resume. Clearing there posts "pushing is safe again", then the
// same run wakes and goes on rewriting the branch — and nothing re-claims,
// because the resume path never reaches markFixInFlight. The all-clear would
// stand for the whole second pass.
//
// An armed retry is not an ending. Same call as the gate lane next door.
// R5fda5a — an armed RetryAfter is only ONE of the ways a run comes back. The
// runner turns a rolling-deploy drain (ErrRunInterrupted), a sandbox phase
// timeout and a capacity refusal into failed_resumable plus a JetStream nak: a
// fresh pod picks the SAME run up with NO RetryAfter ever written. Keying the
// stand-down on RetryAfter announced the fixer done on every drained run —
// ...but a resumable failure with NOTHING armed to resume it IS dead, and its
// While the run is alive the claim is exactly right, and clearing it would
// R65a408 — THE one the first draft of this file could not see. Its
// "foreign status" case used a different DESCRIPTION, so it only ever proved
// that another tool's verdict survives. It never proved that another RUN's
// claim does, and that is the case production actually produces:
//
// the auto-fix lane launches the fixer on the REVIEWER's own head_sha, and two
// consecutive fixer passes reuse it as well. The sweeper re-offers every
// terminal run for the whole lookback — so the reviewer's own reconcile pass,
// terminal and on the same sha, would read the LIVE fixer's claim, match the
// description, and post "the fix run is done" while the branch is still being
// rewritten. A false all-clear, in the exact lane this feature exists for.
//
// R4e92c6 — `pr_url` + `head_sha` are set on EVERY forge-launched run, so
// without a role check before the network the clear pays a live
// ListCommitStatuses for reviewers, branchers, implementers and docs-amenders
// too — on every sweep pass, for the whole lookback, on a path that used to
// exit on a local field read. The assertion is on the READ, not on the write:
// Symmetric to the claim: only ever OUR marker. A verdict another tool posted
// Rbb856d — a DLQ park is FINAL for automation whatever RetryState still says:
// a retry_after can survive on such a doc (the usage-window park that preceded
// an operator resume, when the clear on resume did not land). Standing down on
// it would leave the claim pending for a run nothing will ever wake. The gate
// lane tests this exception FIRST; the first draft here claimed parity with
// R22fa34 — the CLAIM had the defect the clear was fixed for: it overwrote any
// marker of the right shape without asking whose it was. Two fixers share one
// head sha by construction, so the newcomer would take the claim over, and
// whichever run ended first would post the all-clear over the other's live
// ...but its OWN marker is re-claimable: a relaunch of the same run must not be
// locked out by the status it posted itself.
func TestMarkFixInFlight_ReclaimsItsOwnMarker(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStatePending,
			Description: fixInFlightDescription,
			TargetURL:   "https://iterion.test/runs/run-77"},
	}}
	s := fixLaunchFixture(t, gc)

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")

	if gc.setCalls != 1 {
		t.Fatalf("posted %d, want 1 — a run must be able to refresh its own claim", gc.setCalls)
	}
}

// Rfa3481 — after a first pass terminates, the clear leaves `done` on that sha.
// A second `/billy` on an UNCHANGED head — a first pass that BANKED instead of
// pushing, which is the 2026-09-09 incident itself — read its predecessor's
// marker as a foreign verdict and posted nothing. The second pass was then as
// R0839e8 — the clear runs for every terminal forge run the sweeper offers,
// every 60s for a 60-minute lookback, and the role walk is two Mongo reads plus
// a full catalog parse. The memo makes the repeat offers free. Asserted on the
// SECOND call being served without re-walking: a memo nothing reads is just a
// Rf4afa9 — keeping the claim through every resumable park has a counterpart:
// a fixer that parks and never comes back (budget exceeded, retries exhausted,
// a plain execution failure) leaves a pending nobody resolves. The next pass on
// that unchanged head then read it as another run's LIVE claim and stood down —
// the silent second pass again, through the stale-pending door this time.
//
// ...and a claim whose owner is STILL RUNNING is never taken over. Standing
// down is the conservative direction: taking over on a guess is how a false
// Rd033da — the takeover asked store.IsTerminal() while the clear kept the
// claim through every resumable park. IsTerminal() INCLUDES failed_resumable,
// so the two predicates contradicted each other: a fixer parked on a usage
// window — which does come back — had its LIVE claim taken over, and whichever
// run ended first posted the all-clear over the other's rewrite.
//
// And the agreement is exercised through the SITES, not only the predicate: a

// A SECOND pass on an unchanged head must claim too — the 2026-09-09 incident
// itself (a first pass that BANKED instead of pushing, so the head never moved).
// There is no ownership test on our own kind precisely so this works: refreshing
// a claim with a newer run's URL cannot make it false, because the marker
// asserts nothing about either run still working.
func TestMarkFixInFlight_RefreshesAnExistingClaim(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStatePending,
			Description: fixInFlightDescription,
			TargetURL:   "https://iterion.test/runs/the-previous-pass"},
	}}
	s := fixLaunchFixture(t, gc)

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")

	if gc.setCalls != 1 {
		t.Fatalf("posted %d, want 1 — the second pass stays invisible behind the first one's claim", gc.setCalls)
	}
	if gc.last.TargetURL != "https://iterion.test/runs/run-77" {
		t.Errorf("target = %q, want the newest claimant so a reader lands on a live console", gc.last.TargetURL)
	}
	if !isFixInFlight(gc.last) {
		t.Errorf("refreshed claim is not recognisable as one: %q", gc.last.Description)
	}
}

// The marker states a fact about the PAST and is never retracted, so nothing in
// this file may post a terminal state on that context. A `success` there would
// read "pushing is safe again" — the assertion the engine cannot honour, and
// the one that produced eight findings before it was removed.
func TestMarkFixInFlight_NeverPostsATerminalState(t *testing.T) {
	gc := &listingGateClient{}
	s := fixLaunchFixture(t, gc)

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")

	if gc.last.State != forge.CommitStatePending {
		t.Fatalf("state = %q, want pending — a resolved fixer marker asserts an absence nothing can verify", gc.last.State)
	}
}

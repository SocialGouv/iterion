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
		t.Errorf("status %q is not recognisable as the fixer claim — a later pass could not refresh it", gc.last.Description)
	}
	if gc.last.TargetURL != "https://iterion.test/runs/run-77" {
		t.Errorf("target url = %q, want the live run console — the URL names the most recent claimant so a reader lands somewhere", gc.last.TargetURL)
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
// REVIEWER already claims the gate context and answers it with a verdict;
// marking it here too would put a second, never-answered pending on every
// reviewed head.
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

// A run may always refresh its OWN marker: a relaunch of the same run must not
// be locked out by the status it posted itself.
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

// The merge-queue auto-heal lane publishes its revision under a fixer-only key,
// because `head_sha` also arms the GATE claim and that lane never answers a
// gate. This marker must read both — otherwise the one lane that FORCE-pushes
// the branch, and so the one a concurrent writer most needs warned about, stays
// silently invisible.
func TestMarkFixInFlight_AcceptsTheFixerOnlyRevisionKey(t *testing.T) {
	gc := &listingGateClient{}
	s := fixLaunchFixture(t, gc)
	vars := map[string]string{
		"pr_url":       "https://github.com/o/r/pull/42",
		"fix_head_sha": "aaa111", // no head_sha: the heal lane must not arm the gate
	}

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", vars, "run-77")

	if gc.setCalls != 1 {
		t.Fatalf("posted %d, want 1 — the auto-heal lane stays invisible", gc.setCalls)
	}
	if gc.lastSHA != "aaa111" {
		t.Errorf("posted on %q, want the revision the heal lane published", gc.lastSHA)
	}
}

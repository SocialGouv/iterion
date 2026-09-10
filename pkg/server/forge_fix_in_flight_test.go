package server

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
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

// fixRunFixture builds a terminal fixer run holding the inputs the clear reads.
func fixRunFixture(t *testing.T, gc forgeGateClient, status store.RunStatus) (*Server, *store.Run) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s := newForgeGateTestServer(t, st)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	s.cfg.PublicURL = "https://iterion.test"
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return gc, nil
	}
	run, err := st.CreateRun(context.Background(), "run-77", "branch-improve-loop", map[string]any{
		"pr_url": "https://github.com/o/r/pull/42", "head_sha": "deadbeef",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	// The clear resolves its connection off the RUN's tenant (a terminal
	// sweep has no ambient one), so the fixture must carry the tenant the
	// harness registered its connection under — as a real run always does.
	run.TenantID = "team1"
	run.Status = status
	return s, run
}

// A claim that is never resolved is worse than none: it would tell every later
// reader that a run is working when none is. The terminal clear RESOLVES it
// rather than deleting it — a status that vanishes reads as "never claimed",
// the ambiguity this file exists to remove.
func TestClearFixInFlight_ResolvesOurOwnClaim(t *testing.T) {
	for _, status := range []store.RunStatus{store.RunStatusFinished, store.RunStatusFailed, store.RunStatusCancelled} {
		gc := &listingGateClient{statuses: []forge.CommitStatus{
			{Context: fixInFlightContext, State: forge.CommitStatePending, Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/run-77"},
		}}
		s, run := fixRunFixture(t, gc, status)

		s.clearFixInFlight(context.Background(), run)

		if gc.setCalls != 1 {
			t.Fatalf("%s: posted %d, want 1 — the claim stays pending on a run that is over", status, gc.setCalls)
		}
		if gc.last.State != forge.CommitStateSuccess {
			t.Errorf("%s: state = %q, want success", status, gc.last.State)
		}
		if isFixInFlight(gc.last) {
			t.Errorf("%s: still reads as in-flight after the run ended", status)
		}
	}
}

// While the run is alive the claim is exactly right, and clearing it would
// re-open the window it exists to close.
func TestClearFixInFlight_SilentWhileTheRunIsAlive(t *testing.T) {
	for _, status := range []store.RunStatus{store.RunStatusRunning, store.RunStatusQueued, store.RunStatusPausedWaitingHuman} {
		gc := &listingGateClient{statuses: []forge.CommitStatus{
			{Context: fixInFlightContext, State: forge.CommitStatePending, Description: fixInFlightDescription},
		}}
		s, run := fixRunFixture(t, gc, status)

		s.clearFixInFlight(context.Background(), run)

		if gc.setCalls != 0 {
			t.Errorf("%s: released the claim on a run still in flight", status)
		}
	}
}

// Symmetric to the claim: only ever OUR marker. A verdict another tool posted
// on this context survives the run ending.
func TestClearFixInFlight_LeavesAForeignStatusAlone(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStateFailure, Description: "another tool's verdict"},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFinished)

	s.clearFixInFlight(context.Background(), run)

	if gc.setCalls != 0 {
		t.Fatalf("overwrote a status this server does not own (%d posts)", gc.setCalls)
	}
}

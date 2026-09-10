package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
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
//
// Built by the PRODUCTION var builder rather than by hand. A hand-written stub
// carrying different terms than the real producer is precisely how the as-PR
// lane's missing push_branch stayed invisible: every test agreed with every
// other test, and none of them with fixerPRVars.
func fixLaunchVars() map[string]string {
	v := fixerPRVars("main", "feat/x", "https://github.com/o/r/pull/42", "notes", false, nil)
	v["head_sha"] = "deadbeef"
	v["gate_context"] = "iterion/review"
	return v
}

// fixLaunchVarsAsPR is the SAME builder in as-PR mode: it stamps mr_base and
// leaves push_branch unset, because the fixer opens a separate pull request
// instead of rewriting this branch.
func fixLaunchVarsAsPR() map[string]string {
	v := fixerPRVars("main", "feat/x", "https://github.com/o/r/pull/42", "notes", true, nil)
	v["head_sha"] = "deadbeef"
	return v
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
	if gc.last.State != forge.CommitStateSuccess {
		t.Errorf("state = %q, want success — a pending here blocks an MR merge on GitLab, where the status joins the head pipeline", gc.last.State)
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
		{Context: fixInFlightContext, State: forge.CommitStateSuccess,
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
		{Context: fixInFlightContext, State: forge.CommitStateSuccess,
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

// THE invariant that replaced the whole lifecycle: there is exactly ONE marker
// text, and nothing in this file ever posts a second, reassuring one. The
// "all-clear" class of defect — eight findings on this branch — was a SECOND
// description saying the danger had passed. Its absence is the guarantee, not
// the state the status carries.
func TestMarkFixInFlight_HasExactlyOneMessage(t *testing.T) {
	gc := &listingGateClient{}
	s := fixLaunchFixture(t, gc)

	// Two launches on the same head, as two fixer passes really do.
	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")
	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-88")

	if len(gc.posted) == 0 {
		t.Fatal("nothing posted")
	}
	for i, st := range gc.posted {
		if strings.TrimSpace(st.Description) != fixInFlightDescription {
			t.Fatalf("post %d says %q — a second message is how the all-clear came back every time", i, st.Description)
		}
		if st.State != forge.CommitStateSuccess {
			t.Errorf("post %d state = %q, want success — pending blocks an MR merge on GitLab", i, st.State)
		}
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
	// The heal lane's real shape, from its own builder: push-back mode, plus
	// the revision under the fixer-only key and NO head_sha (that key would
	// also arm the gate claim, which this lane deliberately stood down from).
	vars := fixerPRVars("main", "feat/x", "https://github.com/o/r/pull/42", "ejected from the merge queue", false, nil)
	vars["fix_head_sha"] = "aaa111"

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", vars, "run-77")

	if gc.setCalls != 1 {
		t.Fatalf("posted %d, want 1 — the auto-heal lane stays invisible", gc.setCalls)
	}
	if gc.lastSHA != "aaa111" {
		t.Errorf("posted on %q, want the revision the heal lane published", gc.lastSHA)
	}
}

// Rd5f6d3 — in as-PR mode the fixer opens a SEPARATE pull request against the
// source branch instead of pushing back, so "pushing collides with what it
// pushes back" is simply false. And this marker is never retracted: a false
// claim would stand on that head forever.
//
// The counterpart of stating a fact that is never withdrawn is that it must be
// true when posted.
func TestMarkFixInFlight_SilentInAsPRMode(t *testing.T) {
	gc := &listingGateClient{}
	s := fixLaunchFixture(t, gc)

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVarsAsPR(), "run-77")

	if gc.setCalls != 0 {
		t.Fatalf("claimed a push-back collision for a lane that opens a PR instead (%d posts) — and the claim is never retracted", gc.setCalls)
	}
}

// R5318a3 — THE generalisation of the test above, and the reason the guard is
// positive rather than negative.
//
// `open_mr` is optional: fixerPRVars stamps it, and stampBranchImprovePushBack
// stamps it only for the bot holding the brancher ROLE. A team's second fixer —
// free by design, since a bot inherits this marker by declaring
// `consumes: review` — is launched by /command with a head sha and neither var.
// A guard that stood down only on `open_mr == "true"` read that absence as
// "pushes back" and posted a warning that is false, on a head that never moves,
// with nothing in the design able to retract it.
func TestMarkFixInFlight_SilentWhenNoPushBackIsRouted(t *testing.T) {
	gc := &listingGateClient{}
	s := fixLaunchFixture(t, gc)
	// A /command launch: the invocation carries the PR and the revision, and
	// neither of the two vars that describe what the fixer does with them.
	vars := map[string]string{
		"pr_url":   "https://github.com/o/r/pull/42",
		"head_sha": "deadbeef",
	}

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", vars, "run-77")

	if gc.setCalls != 0 {
		t.Fatalf("claimed for a launch that routes no push-back (%d posts) — absence of open_mr is not evidence of pushing back, and this claim is never retracted", gc.setCalls)
	}
}

// ...and the push-back lane, the one whose commits really do land on this head,
// still claims. This is the assertion that keeps the guard above from being
// satisfied by never posting at all.
func TestMarkFixInFlight_ClaimsWhenItPushesBack(t *testing.T) {
	gc := &listingGateClient{}
	s := fixLaunchFixture(t, gc)
	vars := fixLaunchVars()
	if vars["push_branch"] == "" {
		t.Fatal("fixerPRVars stopped routing a push-back in push-back mode — the claim's whole premise")
	}

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", vars, "run-77")

	if gc.setCalls != 1 {
		t.Fatalf("posted %d, want 1 — the lane that DOES push back must warn", gc.setCalls)
	}
}

// R902a73 — THE wiring test, and the only one here that proves the feature
// exists at all.
//
// Every other test in this file calls markFixInFlight directly, so all of them
// stay green when the ONE production call site — the webhook launch tail — is
// deleted: the bench proves the function is correct, never that it is reached.
// This drives a real fixer launch through the GitHub handler and asserts a
// status actually landed on the pull request.
//
// The auto-heal lane is the subject because it is the one that FORCE-pushes,
// and so the one a concurrent writer most needs warned about.
func TestFixerLaunch_ClaimsThroughTheWebhookTail(t *testing.T) {
	s := newWebhookTestServer(t)
	gc := &listingGateClient{}
	s.cfg.PublicURL = "https://iterion.test"
	// The REAL catalog: the role comes from each bot's manifest, so without it
	// every role reads `unknown` and this test would pass by claiming nothing.
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	s.forgeConnections = forge.NewMemoryConnectionStore()
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "conn1", TenantID: "t1", Provider: forge.ProviderGitHub,
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return gc, nil
	}
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		return "run-heal", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "branch-improve-loop"}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghDequeuedPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	if gc.setCalls != 1 {
		t.Fatalf("the launch tail posted %d statuses, want exactly 1 — a fixer launched for real is visible nowhere", gc.setCalls)
	}
	if gc.last.Context != fixInFlightContext {
		t.Errorf("context = %q, want %q", gc.last.Context, fixInFlightContext)
	}
	if gc.lastSHA != "aaa111" {
		t.Errorf("posted on %q, want the dequeued head the heal is about", gc.lastSHA)
	}
	if !isFixInFlight(gc.last) {
		t.Errorf("status is not recognisable as the claim: %q", gc.last.Description)
	}
	// Rcdaa46's other half, asserted rather than argued: this lane must write
	// the fixer context and NOTHING else. It publishes its revision under
	// `fix_head_sha` precisely so `head_sha` does not also arm markGateInFlight
	// on a repo that pins a shared gate_context — a REQUIRED check this fixer
	// would claim and never answer.
	for _, st := range gc.posted {
		if st.Context != fixInFlightContext {
			t.Errorf("the heal lane also wrote %q — it answers no gate and must claim none", st.Context)
		}
	}
}

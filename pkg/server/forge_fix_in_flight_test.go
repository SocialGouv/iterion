package server

import (
	"context"
	"testing"
	"time"

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
	// And its BotID, which CreateRun's third argument does NOT set: the role
	// is derived from it, so leaving it empty makes every role read `unknown`
	// and every "must not touch" assertion below pass without exercising
	// anything. Measured: three of them did exactly that.
	run.BotID = "branch-improve-loop"
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
// the commonest interruption there is.
func TestClearFixInFlight_SilentOnAResumableParkWithNoRetryArmed(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStatePending,
			Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/run-77"},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFailedResumable)
	run.RetryState = nil // a nak-based auto-resume never arms one

	s.clearFixInFlight(context.Background(), run)

	if gc.setCalls != 0 {
		t.Fatalf("announced the fixer done on a drained run (%d posts) — a fresh pod resumes it behind a green check", gc.setCalls)
	}
}

func TestClearFixInFlight_SilentOnAParkedRunWithAnArmedRetry(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStatePending,
			Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/run-77"},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFailedResumable)
	at := time.Now().UTC().Add(time.Hour)
	run.RetryState = &store.RunRetryState{RetryAfter: &at}

	s.clearFixInFlight(context.Background(), run)

	if gc.setCalls != 0 {
		t.Fatalf("announced the fixer done while its retry is armed (%d posts) — it resumes and keeps rewriting the branch behind a green check", gc.setCalls)
	}
}

// ...but a resumable failure with NOTHING armed to resume it IS dead, and its
// claim must not outlive it. The two halves of the same predicate.
func TestClearFixInFlight_ResolvesOnlyOnADLQPark(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStatePending,
			Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/run-77"},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFailedResumable)
	run.FailureCode = store.FailureDLQParked // the ONE resumable ending

	s.clearFixInFlight(context.Background(), run)

	if gc.setCalls != 1 {
		t.Fatalf("posted %d, want 1 — a DLQ park is exhausted, nothing will ever wake it", gc.setCalls)
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
// Ownership is the target URL, never the shape of the text.
func TestClearFixInFlight_LeavesAnotherRunsClaimAlone(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		// Byte-identical to what this run would post — except whose it is.
		{Context: fixInFlightContext, State: forge.CommitStatePending,
			Description: fixInFlightDescription,
			TargetURL:   "https://iterion.test/runs/some-other-run"},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFinished)

	s.clearFixInFlight(context.Background(), run)

	if gc.setCalls != 0 {
		t.Fatalf("resolved a claim posted by ANOTHER run (%d posts) — a live fixer would be announced done while it is still rewriting the branch", gc.setCalls)
	}
}

// R4e92c6 — `pr_url` + `head_sha` are set on EVERY forge-launched run, so
// without a role check before the network the clear pays a live
// ListCommitStatuses for reviewers, branchers, implementers and docs-amenders
// too — on every sweep pass, for the whole lookback, on a path that used to
// exit on a local field read. The assertion is on the READ, not on the write:
// a guard placed after the round trip would still cost it.
func TestClearFixInFlight_NoForgeTrafficForANonFixer(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStatePending, Description: fixInFlightDescription},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFinished)
	run.BotID = "review-pr" // a reviewer: it never claimed, so it owes no clear

	s.clearFixInFlight(context.Background(), run)

	if gc.listCalls != 0 {
		t.Errorf("read the forge %d time(s) for a non-fixer — this path must cost no network at all", gc.listCalls)
	}
	if gc.setCalls != 0 {
		t.Errorf("a non-fixer posted %d status(es)", gc.setCalls)
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

// Rbb856d — a DLQ park is FINAL for automation whatever RetryState still says:
// a retry_after can survive on such a doc (the usage-window park that preceded
// an operator resume, when the clear on resume did not land). Standing down on
// it would leave the claim pending for a run nothing will ever wake. The gate
// lane tests this exception FIRST; the first draft here claimed parity with
// that lane in a comment while omitting the branch.
func TestClearFixInFlight_ResolvesADLQParkDespiteAStaleRetry(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStatePending,
			Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/run-77"},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFailedResumable)
	at := time.Now().UTC().Add(time.Hour)
	run.RetryState = &store.RunRetryState{RetryAfter: &at} // stale, survived the park
	run.FailureCode = store.FailureDLQParked

	s.clearFixInFlight(context.Background(), run)

	if gc.setCalls != 1 {
		t.Fatalf("posted %d, want 1 — a DLQ-parked run is never resumed, so its claim would sit pending forever", gc.setCalls)
	}
}

// R22fa34 — the CLAIM had the defect the clear was fixed for: it overwrote any
// marker of the right shape without asking whose it was. Two fixers share one
// head sha by construction, so the newcomer would take the claim over, and
// whichever run ended first would post the all-clear over the other's live
// rewrite. The class, fixed at both sites rather than one.
func TestMarkFixInFlight_NeverTakesOverAnotherRunsClaim(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStatePending,
			Description: fixInFlightDescription,
			TargetURL:   "https://iterion.test/runs/an-earlier-fixer"},
	}}
	s := fixLaunchFixture(t, gc)

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")

	if gc.setCalls != 0 {
		t.Fatalf("took over a live claim posted by another run (%d posts) — whichever run ends first would then announce the other done", gc.setCalls)
	}
}

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
// invisible as before this change, in the very lane it was written for.
func TestMarkFixInFlight_ClaimsOverAResolvedMarker(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStateSuccess,
			Description: fixDoneDescription,
			TargetURL:   "https://iterion.test/runs/the-previous-pass"},
	}}
	s := fixLaunchFixture(t, gc)

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")

	if gc.setCalls != 1 {
		t.Fatalf("posted %d, want 1 — a second pass on an unchanged head stays invisible behind its predecessor's done marker", gc.setCalls)
	}
	if !isFixInFlight(gc.last) {
		t.Errorf("claim = %q, want the in-flight marker", gc.last.Description)
	}
}

// R0839e8 — the clear runs for every terminal forge run the sweeper offers,
// every 60s for a 60-minute lookback, and the role walk is two Mongo reads plus
// a full catalog parse. The memo makes the repeat offers free. Asserted on the
// SECOND call being served without re-walking: a memo nothing reads is just a
// map.
func TestFixerRoleCached_SecondLookupIsMemoised(t *testing.T) {
	gc := &listingGateClient{}
	s := fixLaunchFixture(t, gc)

	first := s.fixerRoleCached(context.Background(), "", "branch-improve-loop")
	if first != pauseNoticeRoleFixer {
		t.Fatalf("role = %v, want fixer — the memo must not change the answer", first)
	}
	// Break the underlying walk: only a memo hit can still answer correctly.
	s.cfg.Bots.Paths = []string{t.TempDir()}
	if again := s.fixerRoleCached(context.Background(), "", "branch-improve-loop"); again != pauseNoticeRoleFixer {
		t.Errorf("second lookup = %v, want fixer from the memo — every sweep offer would re-walk the catalog", again)
	}
	// A different bot is a different key, so it really does re-walk (and now
	// finds nothing) — proving the memo is keyed, not a blanket yes.
	if other := s.fixerRoleCached(context.Background(), "", "review-pr"); other == pauseNoticeRoleFixer {
		t.Errorf("an unrelated bot was served the memoised fixer answer")
	}
}

// Rf4afa9 — keeping the claim through every resumable park has a counterpart:
// a fixer that parks and never comes back (budget exceeded, retries exhausted,
// a plain execution failure) leaves a pending nobody resolves. The next pass on
// that unchanged head then read it as another run's LIVE claim and stood down —
// the silent second pass again, through the stale-pending door this time.
//
// The claim's target URL names its owner, so the owner can be asked.
func TestMarkFixInFlight_TakesOverAClaimWhoseRunIsOver(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStatePending,
			Description: fixInFlightDescription,
			TargetURL:   "https://iterion.test/runs/dead-fixer"},
	}}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := fixLaunchFixture(t, gc)
	s.cfg.Store = st
	owner, err := st.CreateRun(context.Background(), "dead-fixer", "branch-improve-loop", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	// GENUINELY over — not a resumable park, which comes back and whose claim
	// must NOT be stolen. The first draft of this test used failed_resumable
	// and so encoded the very confusion Rd033da names.
	owner.Status = store.RunStatusFailed
	if err := st.SaveRun(context.Background(), owner); err != nil {
		t.Fatal(err)
	}

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")

	if gc.setCalls != 1 {
		t.Fatalf("posted %d, want 1 — a stale claim from a dead run silences every later pass on this head", gc.setCalls)
	}
	if gc.last.TargetURL != "https://iterion.test/runs/run-77" {
		t.Errorf("claim still names %q, want the live run", gc.last.TargetURL)
	}
}

// ...and a claim whose owner is STILL RUNNING is never taken over. Standing
// down is the conservative direction: taking over on a guess is how a false
// all-clear gets built.
func TestMarkFixInFlight_LeavesAClaimWhoseRunIsAlive(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStatePending,
			Description: fixInFlightDescription,
			TargetURL:   "https://iterion.test/runs/live-fixer"},
	}}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := fixLaunchFixture(t, gc)
	s.cfg.Store = st
	owner, err := st.CreateRun(context.Background(), "live-fixer", "branch-improve-loop", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	owner.Status = store.RunStatusRunning
	if err := st.SaveRun(context.Background(), owner); err != nil {
		t.Fatal(err)
	}

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")

	if gc.setCalls != 0 {
		t.Fatalf("took over a claim whose run is still rewriting the branch (%d posts)", gc.setCalls)
	}
}

// Rd033da — the takeover asked store.IsTerminal() while the clear kept the
// claim through every resumable park. IsTerminal() INCLUDES failed_resumable,
// so the two predicates contradicted each other: a fixer parked on a usage
// window — which does come back — had its LIVE claim taken over, and whichever
// run ended first posted the all-clear over the other's rewrite.
//
// One definition, both sites. This pins the agreement itself.
func TestFixRunIsOver_AgreesWithWhatKeepsTheClaim(t *testing.T) {
	mk := func(st store.RunStatus, code store.FailureCode) *store.Run {
		return &store.Run{Status: st, FailureCode: code}
	}
	cases := []struct {
		name string
		run  *store.Run
		over bool
	}{
		{"finished", mk(store.RunStatusFinished, ""), true},
		{"failed", mk(store.RunStatusFailed, ""), true},
		{"cancelled", mk(store.RunStatusCancelled, ""), true},
		{"running", mk(store.RunStatusRunning, ""), false},
		// The one that mattered: a park the runner or the retry sweeper brings
		// back is NOT over, whatever IsTerminal() says.
		{"resumable park (comes back)", mk(store.RunStatusFailedResumable, ""), false},
		{"DLQ park (exhausted)", mk(store.RunStatusFailedResumable, store.FailureDLQParked), true},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := fixRunIsOver(c.run); got != c.over {
			t.Errorf("%s: over=%v want %v", c.name, got, c.over)
		}
	}
}

// And the agreement is exercised through the SITES, not only the predicate: a
// fixer parked on a usage window keeps its claim AND is not taken over.
func TestFixInFlight_AResumableParkIsNeitherClearedNorStolen(t *testing.T) {
	// clear side
	gcClear := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStatePending,
			Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/run-77"},
	}}
	s1, run := fixRunFixture(t, gcClear, store.RunStatusFailedResumable)
	s1.clearFixInFlight(context.Background(), run)
	if gcClear.setCalls != 0 {
		t.Errorf("clear released a parked fixer's claim (%d posts)", gcClear.setCalls)
	}

	// takeover side, same park
	gcMark := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContext, State: forge.CommitStatePending,
			Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/parked-fixer"},
	}}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s2 := fixLaunchFixture(t, gcMark)
	s2.cfg.Store = st
	owner, err := st.CreateRun(context.Background(), "parked-fixer", "branch-improve-loop", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	owner.Status = store.RunStatusFailedResumable
	if err := st.SaveRun(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	s2.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")
	if gcMark.setCalls != 0 {
		t.Errorf("a new fixer stole a parked fixer's live claim (%d posts) — it comes back and keeps rewriting", gcMark.setCalls)
	}
}

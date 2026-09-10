package server

import (
	"context"
	"strconv"
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

// statusBoardClient is listingGateClient with a MEMORY: what it is told is
// what it reads back, per context, like the forge. The fixed-list fake proves
// one call in isolation; a lifecycle spanning several runs and both lanes —
// two claims, then one clear — is only meaningful if the second read sees the
// first write.
type statusBoardClient struct {
	listingGateClient
}

func (f *statusBoardClient) SetCommitStatus(ctx context.Context, repo, sha string, st forge.CommitStatus) error {
	if err := f.listingGateClient.SetCommitStatus(ctx, repo, sha, st); err != nil {
		return err
	}
	for i, cur := range f.statuses {
		if cur.Context == st.Context {
			f.statuses[i] = st
			return nil
		}
	}
	f.statuses = append(f.statuses, st)
	return nil
}

func (f *statusBoardClient) stateOf(ctxName string) forge.CommitState {
	for _, st := range f.statuses {
		if st.Context == ctxName {
			return st.State
		}
	}
	return ""
}

func (f *statusBoardClient) countState(want forge.CommitState) int {
	n := 0
	for _, st := range f.statuses {
		if st.State == want {
			n++
		}
	}
	return n
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
	if gc.last.Context != fixInFlightContextFor("run-77") {
		t.Errorf("context = %q, want %q", gc.last.Context, fixInFlightContextFor("run-77"))
	}
}

// The context names the RUN in full. A prefix would be tempting — the studio
// shows runs by their first hex digits — and would be exactly wrong here: run
// ids are UUIDv7, whose leading digits are the high bits of a millisecond
// timestamp, so every run launched within the same ~65s window shares them.
// Two fixers launched seconds apart is the concurrency this per-run context
// exists to keep apart, so an abbreviated context would collapse precisely the
// case it is for.
func TestFixInFlightContextFor_NamesTheRunInFull(t *testing.T) {
	a := "019f8384-1000-7000-8000-aaaaaaaaaaaa"
	b := "019f8384-1000-7000-8000-bbbbbbbbbbbb" // same ms-timestamp prefix
	if fixInFlightContextFor(a) == fixInFlightContextFor(b) {
		t.Fatalf("two runs from the same time window share context %q — one would resolve the other's warning", fixInFlightContextFor(a))
	}
	if fixInFlightContextFor("") != "" {
		t.Errorf("a run with no id got context %q, want none — an unattributable claim can never be resolved", fixInFlightContextFor(""))
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
			{Context: fixInFlightContextFor("run-77"), State: st, Description: "something another tool posted"},
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
			{Context: fixInFlightContextFor("run-77"), State: forge.CommitStatePending, Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/run-77"},
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
		// The POSTED marker, not the constant: descriptions go out through
		// TruncateStatusDescription, and a resolved marker the claim cannot
		// recognise reads to it as a foreign verdict — so a later fixer on
		// that head would stand down and stay invisible. The claim's own
		// round trip is asserted the same way in ClaimsForAFixer.
		if !isFixDone(gc.last) {
			t.Errorf("%s: released marker %q is not recognisable as ours", status, gc.last.Description)
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
func TestClearFixInFlight_SilentOnAParkedRunWithAnArmedRetry(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContextFor("run-77"), State: forge.CommitStatePending,
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
func TestClearFixInFlight_ResolvesAParkedRunNothingWillResume(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContextFor("run-77"), State: forge.CommitStatePending,
			Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/run-77"},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFailedResumable)
	run.RetryState = nil

	s.clearFixInFlight(context.Background(), run)

	if gc.setCalls != 1 {
		t.Fatalf("posted %d, want 1 — a run nothing will resume leaves its claim pending forever", gc.setCalls)
	}
}

// R5fda5a — the armed retry is only ONE of the parks that auto-resume, and it
// was the only one the guard knew. A drain (a rolling deploy interrupting a
// run mid-flight), a sandbox setup timeout and a capacity park all land on
// failed_resumable with NO RetryAfter ever written: the runner Naks, a fresh
// pod picks the SAME run back up, and nothing re-claims on the way. The clear
// posted "pushing is safe again" over that whole second pass — the false
// all-clear this file calls worse than silence, on the paths that produce it
// most often.
//
// The continuation is the repo's own answer to "does anything own this run's
// future", promoted by the runner at the actual Nak.
func TestClearFixInFlight_SilentOnAParkAContinuationWillResume(t *testing.T) {
	for name, state := range map[string]store.ContinuationState{
		// A drain / sandbox-timeout / capacity park: Nak'd, redelivered,
		// no RetryState anywhere in the picture.
		"queue redelivery": store.ContinuationRedeliveryPending,
		// A quota park, now stated typed rather than inferred from RetryAfter.
		"armed retry": store.ContinuationRetryArmed,
	} {
		gc := &listingGateClient{statuses: []forge.CommitStatus{
			{Context: fixInFlightContextFor("run-77"), State: forge.CommitStatePending,
				Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/run-77"},
		}}
		s, run := fixRunFixture(t, gc, store.RunStatusFailedResumable)
		run.ContinuationState = state
		run.RetryState = nil // the whole point: nothing armed, and it still resumes

		s.clearFixInFlight(context.Background(), run)

		if gc.setCalls != 0 {
			t.Errorf("%s: announced the fixer done (%d posts) — the same run resumes and keeps rewriting the branch behind a green check", name, gc.setCalls)
		}
	}
}

// ...and the other side of that predicate: an interrupted run on its LAST
// permitted delivery "Naks into nothing" — the runner leaves the continuation
// unknown on purpose, because nobody owns its future. A rule keyed on the
// failure code (interrupted ⇒ will resume) would strand that dead run's claim
// pending forever. Unknown clears.
func TestClearFixInFlight_ResolvesAParkNothingOwns(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContextFor("run-77"), State: forge.CommitStatePending,
			Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/run-77"},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFailedResumable)
	run.FailureCode = store.FailureInterrupted
	run.ContinuationState = "" // Nak'd into nothing: unknown, and nothing wakes it
	run.RetryState = nil

	s.clearFixInFlight(context.Background(), run)

	if gc.setCalls != 1 {
		t.Fatalf("posted %d, want 1 — a run nothing will resume leaves its claim pending forever", gc.setCalls)
	}
}

// While the run is alive the claim is exactly right, and clearing it would
// re-open the window it exists to close.
func TestClearFixInFlight_SilentWhileTheRunIsAlive(t *testing.T) {
	for _, status := range []store.RunStatus{store.RunStatusRunning, store.RunStatusQueued, store.RunStatusPausedWaitingHuman} {
		gc := &listingGateClient{statuses: []forge.CommitStatus{
			{Context: fixInFlightContextFor("run-77"), State: forge.CommitStatePending, Description: fixInFlightDescription},
		}}
		s, run := fixRunFixture(t, gc, status)

		s.clearFixInFlight(context.Background(), run)

		if gc.setCalls != 0 {
			t.Errorf("%s: released the claim on a run still in flight", status)
		}
	}
}

// R65a408 — the second half of ownership, kept after the context became
// per-run. Its "foreign status" sibling uses a different DESCRIPTION, so it
// only ever proves another TOOL's verdict survives; this one proves another
// RUN's claim does. Ownership is the target URL, never the shape of the text —
// the same test the gate lane applies, so the two markers cannot diverge on the
// one question that decides whether a green check is a lie.
func TestClearFixInFlight_LeavesAnotherRunsClaimAlone(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		// Byte-identical to what this run would post — except whose it is.
		{Context: fixInFlightContextFor("run-77"), State: forge.CommitStatePending,
			Description: fixInFlightDescription,
			TargetURL:   "https://iterion.test/runs/some-other-run"},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFinished)

	s.clearFixInFlight(context.Background(), run)

	if gc.setCalls != 0 {
		t.Fatalf("resolved a claim posted by ANOTHER run (%d posts) — a live fixer would be announced done while it is still rewriting the branch", gc.setCalls)
	}
}

// THE WIRING. Every test above calls clearFixInFlight directly, and in
// production exactly one call site reaches it: the gate reconciler, fed by the
// run-outcome event and by the 60s sweep. Move or drop that line and every
// claim this feature posts sits pending forever, with nothing failing.
//
// Its POSITION is the assertion, not merely its presence. A fixer holds no
// gate_context and — in this fixture, as for any lane whose grant expired —
// no publish grant either, so the reconciler bows out at "not a gating run" a
// few lines below. The release has to have happened before that, and before
// every other stand-down in that function.
func TestReconcileGate_ReleasesTheFixerClaimBeforeStandingDown(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContextFor("run-77"), State: forge.CommitStatePending,
			Description: fixInFlightDescription, TargetURL: "https://iterion.test/runs/run-77"},
	}}
	s, run := fixRunFixture(t, gc, store.RunStatusFinished)
	// The reconciler loads the run by id, so the fixture's in-memory tenant,
	// bot and status have to be on the stored document.
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save run: %v", err)
	}

	if err := s.reconcileGateForRunID(context.Background(), run.ID, gateTriggerSweep); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if gc.setCalls != 1 {
		t.Fatalf("posted %d statuses, want 1 — the fixer's claim is released by this path and no other", gc.setCalls)
	}
	if !isFixDone(gc.last) {
		t.Errorf("posted %q on %q, want the fixer's released marker", gc.last.Description, gc.last.Context)
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
		{Context: fixInFlightContextFor("run-77"), State: forge.CommitStatePending, Description: fixInFlightDescription},
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
		{Context: fixInFlightContextFor("run-77"), State: forge.CommitStateFailure, Description: "another tool's verdict"},
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
		{Context: fixInFlightContextFor("run-77"), State: forge.CommitStatePending,
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

// R22fa34 — the CLAIM keeps the same ownership test as the clear: a live
// warning that speaks for another run is never blanked, whatever it is written
// on. Symmetry between the two sites is the point — a marker one lane will
// write over and the other will not is how a claim ends up unresolvable.
func TestMarkFixInFlight_NeverTakesOverAnotherRunsClaim(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContextFor("run-77"), State: forge.CommitStatePending,
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
		{Context: fixInFlightContextFor("run-77"), State: forge.CommitStatePending,
			Description: fixInFlightDescription,
			TargetURL:   "https://iterion.test/runs/run-77"},
	}}
	s := fixLaunchFixture(t, gc)

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")

	if gc.setCalls != 1 {
		t.Fatalf("posted %d, want 1 — a run must be able to refresh its own claim", gc.setCalls)
	}
}

// Rfa3481 — a RESOLVED marker is claimable over. Since the context became
// per-run, the case that reaches this branch is a run whose claim was released
// on a park nothing owned (a DLQ park, an unknown continuation) and which an
// operator then resumed: the resumed pass would otherwise stay invisible behind
// its own `done` marker — as invisible as before this change, in the very lane
// it was written for. `done` means no fixer is working, which a launch is about
// to make false.
func TestMarkFixInFlight_ClaimsOverAResolvedMarker(t *testing.T) {
	gc := &listingGateClient{statuses: []forge.CommitStatus{
		{Context: fixInFlightContextFor("run-77"), State: forge.CommitStateSuccess,
			Description: fixDoneDescription,
			TargetURL:   "https://iterion.test/runs/run-77"},
	}}
	s := fixLaunchFixture(t, gc)

	s.markFixInFlight(context.Background(), "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")

	if gc.setCalls != 1 {
		t.Fatalf("posted %d, want 1 — a resumed pass stays invisible behind the done marker its own park left", gc.setCalls)
	}
	if !isFixInFlight(gc.last) {
		t.Errorf("claim = %q, want the in-flight marker", gc.last.Description)
	}
}

// THE lifecycle the shared context could not express, end to end on one head
// sha. Two fixers there is not exotic: each `/billy` comment carries its own
// idempotency key, the auto-fix lane launches on the reviewer's own head, the
// merge-queue auto-heal on the dequeued one — and overlap=supersede, the only
// thing that would cancel the older run, is per-bot AND off unless a webhook
// sets it.
//
// On one shared row the claimant's terminal clear posted "pushing is safe
// again" while its sibling was still rewriting the branch: a green check on the
// exact collision this feature exists to warn about. Standing the second run
// down only moved the hole to the other end. A row per run is what makes both
// halves true at once.
func TestFixInFlight_ConcurrentFixersOnOneHead(t *testing.T) {
	gc := &statusBoardClient{}
	s := fixLaunchFixture(t, gc)
	ctx := context.Background()

	// Both fixers launch on the same revision.
	s.markFixInFlight(ctx, "team1", "", "branch-improve-loop", fixLaunchVars(), "run-77")
	s.markFixInFlight(ctx, "team1", "", "branch-improve-loop", fixLaunchVars(), "run-88")
	if n := gc.countState(forge.CommitStatePending); n != 2 {
		t.Fatalf("%d live warnings for 2 live fixers — a run nobody can see is the whole defect", n)
	}

	// The FIRST one ends. Its own row resolves; the other's warning must stand.
	sc, first := fixRunFixture(t, gc, store.RunStatusFinished)
	sc.clearFixInFlight(ctx, first)

	if got := gc.stateOf(fixInFlightContextFor("run-88")); got != forge.CommitStatePending {
		t.Fatalf("the second fixer's warning is %q, want pending — a reader about to push is told the branch is free while it is still being rewritten", got)
	}
	if got := gc.stateOf(fixInFlightContextFor("run-77")); got != forge.CommitStateSuccess {
		t.Errorf("the finished fixer's own row is %q, want success — its claim would sit pending forever", got)
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

// The memo's TTL expires an entry but never removes it: only a re-lookup of
// the SAME key overwrites one, so a key seen once is held for the life of the
// process. On a long-lived multi-tenant server the key space is (tenant × bot)
// and the climb is one-way — the shape nobody notices until they read a heap
// profile. The cap is what makes it a cache instead of a ledger.
func TestFixerRoleCached_MemoIsBounded(t *testing.T) {
	for name, expires := range map[string]time.Time{
		// The ordinary tail: keys walked once, long past their TTL, that no
		// re-lookup will ever overwrite.
		"expired entries are reclaimed": time.Now().Add(-time.Hour),
		// And the case where reclaiming frees nothing — every key still live.
		// A memo is a performance filter, so dropping it whole costs one
		// re-walk per live key and never a wrong answer.
		"a full live memo is reset": time.Now().Add(time.Hour),
	} {
		gc := &listingGateClient{}
		s := fixLaunchFixture(t, gc)
		s.fixRoleMemo = make(map[string]fixRoleEntry, fixRoleMemoMax)
		for i := 0; i < fixRoleMemoMax; i++ {
			s.fixRoleMemo["tenant-"+strconv.Itoa(i)+"|some-bot"] = fixRoleEntry{expires: expires}
		}

		role := s.fixerRoleCached(context.Background(), "", "branch-improve-loop")

		if role != pauseNoticeRoleFixer {
			t.Errorf("%s: role = %v, want fixer — the bound must not change any answer", name, role)
		}
		s.fixRoleMu.Lock()
		n := len(s.fixRoleMemo)
		s.fixRoleMu.Unlock()
		if n > fixRoleMemoMax {
			t.Errorf("%s: memo holds %d entries, cap is %d — it climbs for the life of the process", name, n, fixRoleMemoMax)
		}
	}
}

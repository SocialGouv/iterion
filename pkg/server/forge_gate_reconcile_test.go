package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// listingGateClient is a fakeGateClient that can also be read from — the
// capability the reconciler needs to tell "no verdict" from "already posted".
type listingGateClient struct {
	fakeGateClient
	statuses  []forge.CommitStatus
	listErr   error
	listCalls int
}

func (f *listingGateClient) ListCommitStatuses(context.Context, string, string) ([]forge.CommitStatus, error) {
	f.listCalls++
	return f.statuses, f.listErr
}

// gateReconcileFixture wires a server with a store holding one run, a publish
// grant, and a stub forge.
func gateReconcileFixture(t *testing.T, inputs map[string]any, gc forgeGateClient) (*Server, string) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s := newForgeGateTestServer(t, st)
	const runID = "run-gating"
	if _, err := st.CreateRun(context.Background(), runID, "review_pr", inputs); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	registerPublishToken(t, s, "tok-gate", ForgePublishGrant{
		TeamID: "team1", ConnectionID: "conn1", Repo: "o/r", Bot: "review-pr",
	})
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return gc, nil
	}
	return s, runID
}

func newForgeGateTestServer(t *testing.T, st store.RunStore) *Server {
	t.Helper()
	s, _ := newForgePublishTestServer(t)
	s.cfg.Store = st
	s.cfg.PublicURL = "https://iterion.test"
	return s
}

func terminalEvent(runID string) trigger.Event {
	return trigger.Event{
		Source:  trigger.SourceRun,
		Kind:    trigger.KindRunCancelled,
		Subject: trigger.Subject{ID: runID},
	}
}

func gatingInputs() map[string]any {
	return map[string]any{
		"pr_url":                "https://github.com/o/r/pull/42",
		forgePublishVarToken:    "tok-gate",
		"gate_context":          "iterion/review",
		"head_sha":              "deadbeef",
		"forge_publish_url":     "https://iterion.test/api/v1/forge/publish-review",
		"unrelated_other_thing": 1,
	}
}

// The whole point: a run that owed a verdict and died must leave one. An
// absent required check is indistinguishable from one still running, so the
// PR waits forever on a context that will never arrive — no error on the run,
// the PR or the check.
func TestGateReconcile_InterruptedRunPostsAFailure(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)

	if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gc.setCalls != 1 {
		t.Fatalf("posted %d statuses, want 1 — the PR is left waiting on a check that never arrives", gc.setCalls)
	}
	if gc.last.Context != "iterion/review" {
		t.Errorf("context = %q, want the gate the run owed", gc.last.Context)
	}
	if gc.last.State != forge.CommitStateFailure {
		t.Errorf("state = %q, want failure — a review that did not happen has approved nothing", gc.last.State)
	}
	if gc.last.Description == "" {
		t.Error("no description: whoever finds this check has no other clue about what happened")
	}
	if gc.last.TargetURL == "" {
		t.Error("no target url: the check should lead to the run that owed it")
	}
	if gc.lastSHA != "deadbeef" {
		t.Errorf("posted on %q, want the PR head", gc.lastSHA)
	}
}

// A run that published normally already left its verdict. Re-posting would
// overwrite a real success with a synthetic failure — strictly worse than the
// bug being fixed.
func TestGateReconcile_LeavesAPostedVerdictAlone(t *testing.T) {
	gc := &listingGateClient{
		fakeGateClient: fakeGateClient{headSHA: "deadbeef"},
		statuses:       []forge.CommitStatus{{Context: "iterion/review", State: forge.CommitStateSuccess}},
	}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)

	if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gc.setCalls != 0 {
		t.Fatalf("overwrote a verdict that was already posted (%d writes)", gc.setCalls)
	}
}

// Cannot read the statuses → cannot tell absent from posted → must not write.
func TestGateReconcile_AbstainsWhenItCannotRead(t *testing.T) {
	t.Run("read fails", func(t *testing.T) {
		gc := &listingGateClient{
			fakeGateClient: fakeGateClient{headSHA: "deadbeef"},
			listErr:        errors.New("forge unreachable"),
		}
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)
		_ = s.reconcileGateForRun(context.Background(), terminalEvent(runID))
		if gc.setCalls != 0 {
			t.Fatalf("wrote a verdict without being able to check for one (%d writes)", gc.setCalls)
		}
	})

	t.Run("provider cannot list at all", func(t *testing.T) {
		gc := &fakeGateClient{headSHA: "deadbeef"} // no ListCommitStatuses
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)
		_ = s.reconcileGateForRun(context.Background(), terminalEvent(runID))
		if gc.setCalls != 0 {
			t.Fatalf("wrote a verdict on a provider it cannot read back (%d writes)", gc.setCalls)
		}
	})
}

// Most runs owe nothing. Touching a PR for one of those would be a bug of its
// own — the reconciler would be posting checks nobody asked for.
func TestGateReconcile_IgnoresRunsThatOweNothing(t *testing.T) {
	// The half-configured shape: gate_context still pinned, gate DISABLED by
	// an explicit gate_enabled pin. The run never owed a verdict — a
	// synthetic failure here would manufacture the deadlock the pin avoids.
	gateOff := gatingInputs()
	gateOff["gate_enabled"] = "false"
	gateOffBool := gatingInputs()
	gateOffBool["gate_enabled"] = false
	for _, tc := range []struct {
		name   string
		inputs map[string]any
	}{
		{"no publish grant", map[string]any{"pr_url": "https://github.com/o/r/pull/42"}},
		{"no pr", map[string]any{forgePublishVarToken: "tok-gate"}},
		{"nothing at all", map[string]any{}},
		{"gate disabled by pin", gateOff},
		{"gate disabled by bool pin", gateOffBool},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
			s, runID := gateReconcileFixture(t, tc.inputs, gc)
			_ = s.reconcileGateForRun(context.Background(), terminalEvent(runID))
			if gc.setCalls != 0 {
				t.Fatalf("posted a status for a run that owed none (%d writes)", gc.setCalls)
			}
		})
	}
}

// Holding a publish grant is not owing a verdict. The server mints one for
// ANY bot launched with a pr_url — the brancher, the docs amender, the
// implementer — and a repo's gate context is deliberately SHARED between the
// bots that gate it. Without an explicit anchor, a bot that owes nothing would
// paint another bot's required check red, which is a worse outage than the one
// being repaired.
func TestGateReconcile_RefusesToSpeakForAnUnnamedRevision(t *testing.T) {
	inputs := gatingInputs()
	delete(inputs, "head_sha") // a launch path that never stamped one
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, inputs, gc)
	_ = s.reconcileGateForRun(context.Background(), terminalEvent(runID))
	if gc.setCalls != 0 {
		t.Fatalf("spoke for a revision the run never named (%d writes)", gc.setCalls)
	}
}

func TestGateReconcile_NeedsThePinnedContextToActAtAll(t *testing.T) {
	inputs := gatingInputs()
	delete(inputs, "gate_context")

	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, inputs, gc)
	_ = s.reconcileGateForRun(context.Background(), terminalEvent(runID))
	if gc.setCalls != 0 {
		t.Fatalf("posted a context nobody pinned for this repo (%d writes)", gc.setCalls)
	}
}

// A paused run is expected to resume and post its own verdict.
func TestGateReconcile_LeavesPausedRunsAlone(t *testing.T) {
	for _, st := range []store.RunStatus{
		store.RunStatusPausedWaitingHuman,
		store.RunStatusPausedOperator,
	} {
		t.Run(string(st), func(t *testing.T) {
			gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
			s, runID := gateReconcileFixture(t, gatingInputs(), gc)
			if err := s.cfg.Store.UpdateRunStatus(context.Background(), runID, st, ""); err != nil {
				t.Fatal(err)
			}
			_ = s.reconcileGateForRun(context.Background(), terminalEvent(runID))
			if gc.setCalls != 0 {
				t.Fatalf("%s: wrote a verdict over a run that is expected to post its own (%d writes)", st, gc.setCalls)
			}
		})
	}
}

// A resumable failure is only "not dead" while a retry is actually ARMED. The
// runner arms one for usage-window failures (persisted before the outcome
// event fires); everything else — budget exceeded, exhausted attempts, a
// plain execution failure — has nothing coming back for it, and skipping
// those left a PR silently unmergeable behind an absent required check
// (observed in production, Vetty run 019fc8e5 on 2026-08-03).
func TestGateReconcile_FailedResumable(t *testing.T) {
	setStatus := func(t *testing.T, s *Server, runID string, retry *store.RunRetryState, runErr string) {
		t.Helper()
		run, err := s.cfg.Store.LoadRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		run.Status = store.RunStatusFailedResumable
		run.Error = runErr
		run.RetryState = retry
		if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("an armed retry stands the reconciler down", func(t *testing.T) {
		gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)
		at := time.Now().UTC().Add(time.Hour)
		setStatus(t, s, runID, &store.RunRetryState{RetryAfter: &at, Reason: "usage_window"}, "usage window shut")
		_ = s.reconcileGateForRun(context.Background(), terminalEvent(runID))
		if gc.setCalls != 0 {
			t.Fatalf("wrote a verdict over a run whose retry is armed and about to resume (%d writes)", gc.setCalls)
		}
	})

	t.Run("no retry armed means the run is dead — reconcile it", func(t *testing.T) {
		gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)
		setStatus(t, s, runID, nil, "budget exceeded: duration (2401987036905/2400000000000)")
		if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if gc.setCalls != 1 {
			t.Fatalf("posted %d statuses, want 1 — nothing resumes a budget-exceeded run on its own", gc.setCalls)
		}
		if gc.last.State != forge.CommitStateFailure {
			t.Errorf("state = %q, want failure", gc.last.State)
		}
		// The reason is on the check: whoever finds it must not have to dig
		// through run storage to learn WHY the review died.
		if !strings.Contains(gc.last.Description, "budget exceeded") {
			t.Errorf("description %q does not carry the failure reason", gc.last.Description)
		}
		if !isSyntheticGateInterruption(gc.last.Description) {
			t.Errorf("description %q is not recognizable as synthetic — the auto-fix lane would treat it as a real verdict", gc.last.Description)
		}
	})

	t.Run("an abandoned retry is dead too", func(t *testing.T) {
		gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)
		setStatus(t, s, runID, &store.RunRetryState{
			RetryAfter: nil, Reason: "usage_window", Attempts: 3,
			LastError: "usage-window retries exhausted (max 3)",
		}, "usage limit blocked")
		_ = s.reconcileGateForRun(context.Background(), terminalEvent(runID))
		if gc.setCalls != 1 {
			t.Fatalf("posted %d statuses, want 1 — an abandoned retry never comes back", gc.setCalls)
		}
	})
}

// A long reason is truncated on a RUNE boundary: provider prose carries
// accents, and invalid UTF-8 in a status description is a forge 422 — a long
// reason must never cost the synthetic status itself.
func TestGateReconcile_ReasonTruncationIsRuneSafe(t *testing.T) {
	long := "budget exceeded: durée écoulée éééééééééééééééééééééééé — provider était injoignable pendant la fenêtre"
	got := gateInterruptedDescriptionFor(&store.Run{Error: long})
	if !utf8.ValidString(got) {
		t.Fatalf("truncated description is not valid UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, gateDiedDescriptionPrefix) || !strings.Contains(got, "…") {
		t.Errorf("unexpected shape: %q", got)
	}
	if !isSyntheticGateInterruption(got) {
		t.Errorf("truncated description no longer recognized as synthetic: %q", got)
	}
}

// The reason on the synthetic status must survive the queue/runner wrappers:
// the raw error of a runner reject reads "max deliveries exhausted: runner:
// prepare repo workspace for <id>: runner: reject repo ref: …" and the
// 60-rune budget used to truncate before the only actionable part.
func TestGateReconcile_ReasonStripsMechanicalWrappers(t *testing.T) {
	run := &store.Run{Error: `max deliveries exhausted: runner: prepare repo workspace for 01a0321a-7945-7dfe-a886-0cb56054caa4: runner: reject repo ref: git: branch name "renovate/npm-(non-major)" must match [A-Za-z0-9][A-Za-z0-9._/-]* (parked on DLQ — replay via /api/admin/dlq)`}
	got := gateInterruptedDescriptionFor(run)
	if !strings.Contains(got, `reject repo ref`) || !strings.Contains(got, "renovate/npm-(non-major)") {
		t.Errorf("description lost the actionable cause: %q", got)
	}
	if strings.Contains(got, "max deliveries exhausted") || strings.Contains(got, "prepare repo workspace") {
		t.Errorf("description still carries mechanical wrappers: %q", got)
	}
	if !isSyntheticGateInterruption(got) {
		t.Errorf("stripped description no longer recognized as synthetic: %q", got)
	}
}

// The synthetic marker must be recognized in every shape the reconciler
// writes and must never swallow a real verdict.
func TestGateReconcile_SyntheticMarker(t *testing.T) {
	for _, tc := range []struct {
		desc string
		want bool
	}{
		{gateInterruptedDescription, true},
		{"review died (budget exceeded: duration…) — push again or comment the bot's command to re-run", true},
		{gateDLQDescription, true},
		{"no blocking findings (≥high); 4 total", false},
		{"supply-chain audit clean; no alignment needed, build verified", false},
		{"", false},
	} {
		if got := isSyntheticGateInterruption(tc.desc); got != tc.want {
			t.Errorf("isSyntheticGateInterruption(%q) = %v, want %v", tc.desc, got, tc.want)
		}
	}
}

// The head moves while a run is alive — the author pushes a fix, a brancher
// commits, review_on_sync starts a fresh review. Reporting on the CURRENT head
// would red-flag a commit the dead run never read, and a newer head is a newer
// review's responsibility.
func TestGateReconcile_DoesNotSpeakForAHeadItNeverReviewed(t *testing.T) {
	inputs := gatingInputs()
	inputs["head_sha"] = "0ldc0mmit"

	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, inputs, gc)
	_ = s.reconcileGateForRun(context.Background(), terminalEvent(runID))
	if gc.setCalls != 0 {
		t.Fatalf("posted on a head this run never reviewed (%d writes)", gc.setCalls)
	}

	inputs["head_sha"] = "deadbeef"
	s2, runID2 := gateReconcileFixture(t, inputs, gc)
	if err := s2.reconcileGateForRun(context.Background(), terminalEvent(runID2)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gc.setCalls != 1 {
		t.Fatalf("the head it DID review must still get its verdict (%d writes)", gc.setCalls)
	}
}

// A pull request that is closed or merged owes nobody a verdict. The
// reconciler used to paint its synthetic failure there anyway — a red
// "review died — push again or comment the bot's command to re-run" on a
// pull request already merged, telling a developer to re-run a review of
// work that shipped. Reachable from every terminal outcome on such a run:
// the retry sweeper's abandon republishes the outcome deliberately, the
// stop-on-close cancel is itself a terminal outcome, and the sweep re-offers
// the run for its whole lookback.
//
// Same predicate the relaunch and auto-fix lanes already use: an EMPTY state
// (a provider that does not report one) is not a closure, so a verdict is
// never suppressed on an unknown.
func TestGateReconcile_ClosedPullRequestGetsNoSyntheticFailure(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  int
	}{
		{"open", 1},
		{"", 1}, // unknown state: never suppress a verdict on a guess
		{"closed", 0},
		{"merged", 0},
	} {
		t.Run("state="+tc.state, func(t *testing.T) {
			gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef", state: tc.state}}
			s, runID := gateReconcileFixture(t, gatingInputs(), gc)
			run, err := s.cfg.Store.LoadRun(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			run.Status = store.RunStatusCancelled
			run.Error = "auto-retry abandoned: monthly_run_quota_exceeded"
			if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
				t.Fatal(err)
			}

			if err := s.reconcileGateForRunID(context.Background(), runID, gateTriggerEvent); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if gc.setCalls != tc.want {
				t.Fatalf("pull request %q: posted %d statuses, want %d (last: %q %q)",
					tc.state, gc.setCalls, tc.want, gc.last.State, gc.last.Description)
			}
		})
	}
}

// declining is an ANSWER, not a death. The relaunch lane already stands down
// on the typed code; this reader was left behind and painted "review died …
// push again or comment the bot's command to re-run" on a head where a bot
// deliberately changed nothing — advice that re-derives the same refusal, and
// a lie about what happened.
func TestGateReconcile_DeclinedRunLeavesNoDeadReviewWording(t *testing.T) {
	t.Run("no verdict on the head means say the decline, not a death", func(t *testing.T) {
		gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)
		run, err := s.cfg.Store.LoadRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		run.Status = store.RunStatusFailed
		run.FailureCode = declinedFailureCode
		run.Error = "the queue ejected this PR on an unrelated flaky test; the diff has no defect"
		if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		if err := s.reconcileGateForRunID(context.Background(), runID, gateTriggerEvent); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if gc.setCalls != 1 {
			t.Fatalf("the check still has to be answered — a pull request left on a claim nobody resolves is the bug this repair exists for (writes=%d)", gc.setCalls)
		}
		if strings.Contains(gc.last.Description, "review died") || strings.Contains(gc.last.Description, "push again") {
			t.Fatalf("a decline is not a dead review and a push does not change it: %q", gc.last.Description)
		}
		if !strings.Contains(strings.ToLower(gc.last.Description), "declined") {
			t.Fatalf("the description must name what actually happened: %q", gc.last.Description)
		}
		if !isSyntheticGateInterruption(gc.last.Description) {
			t.Fatalf("the decline status is one of ours: a later pass that cannot recognise it treats it as a real verdict and goes silent — %q", gc.last.Description)
		}
	})

	t.Run("a verdict already on the head is left alone", func(t *testing.T) {
		gc := &listingGateClient{
			fakeGateClient: fakeGateClient{headSHA: "deadbeef"},
			statuses:       []forge.CommitStatus{{Context: "iterion/review", State: forge.CommitStateFailure, Description: "2 blocking finding(s) ≥high"}},
		}
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)
		run, err := s.cfg.Store.LoadRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		run.Status = store.RunStatusFailed
		run.FailureCode = declinedFailureCode
		run.Error = "nothing to fix in this diff"
		if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		if err := s.reconcileGateForRunID(context.Background(), runID, gateTriggerEvent); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if gc.setCalls != 0 {
			t.Fatalf("the reviewer's verdict from before the fixer ran is still the truth (%d writes: %q)", gc.setCalls, gc.last.Description)
		}
	})
}

// A review that died at its OWN cost cap does not come back by pushing: the
// next run reaches the same cap on the same diff. The status has to say so as
// a verdict and route to a human, instead of offering a remedy that
// reproduces the death (#788).
func TestGateReconcile_BudgetExceededVerdictRoutesToAHuman(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	run, err := s.cfg.Store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusFailedResumable
	run.FailureCode = store.FailureBudgetExceeded
	run.Error = "budget exceeded: cost_usd (36/12)"
	if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileGateForRunID(context.Background(), runID, gateTriggerEvent); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gc.setCalls != 1 {
		t.Fatalf("the check must be answered, writes=%d", gc.setCalls)
	}
	d := gc.last.Description
	if strings.Contains(d, "push again") {
		t.Fatalf("pushing again reproduces the death — the remedy must not be the disease: %q", d)
	}
	if !strings.Contains(d, "36") || !strings.Contains(d, "12") {
		t.Fatalf("the spend and the cap are the two numbers a human decides on: %q", d)
	}
	if !strings.Contains(strings.ToLower(d), "human") {
		t.Fatalf("a gate that cannot afford its review has no exit but a human — say it: %q", d)
	}
	if !isSyntheticGateInterruption(d) {
		t.Fatalf("the budget verdict is one of ours and must stay recognisable: %q", d)
	}
}

// Two deaths on the same cap are the automation's whole budget: the relaunch
// replays the same review of the same diff against the same cap, so a third
// launch is a third $30 for the same answer.
func TestGateRelaunch_StandsDownOnASecondBudgetDeath(t *testing.T) {
	inputs := gatingInputs()
	inputs[gateRelaunchOfVar] = "run-that-also-died-on-budget"
	run := &store.Run{
		ID: "run-relaunched", Status: store.RunStatusFailedResumable,
		FailureCode: store.FailureBudgetExceeded,
		Error:       "budget exceeded: cost_usd (30/12)",
		Inputs:      inputs,
	}
	if !gateRelaunchIsSpentOnBudget(run) {
		t.Fatal("a relaunch that died on the same cap as the run it replaced must stop the automation")
	}
	first := &store.Run{
		ID: "run-first", Status: store.RunStatusFailedResumable,
		FailureCode: store.FailureBudgetExceeded,
		Error:       "budget exceeded: cost_usd (36/12)",
		Inputs:      gatingInputs(),
	}
	if gateRelaunchIsSpentOnBudget(first) {
		t.Fatal("the FIRST budget death still gets its one relaunch — a transient overrun is real")
	}
	other := &store.Run{
		ID: "run-other", Status: store.RunStatusFailedResumable,
		FailureCode: store.FailureExecutionFailed,
		Error:       "provider unreachable",
		Inputs:      inputs,
	}
	if gateRelaunchIsSpentOnBudget(other) {
		t.Fatal("a relaunch that died of something else keeps the ordinary recovery")
	}
}

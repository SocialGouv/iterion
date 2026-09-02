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

// The synthetic marker must be recognized in both its shapes and must never
// swallow a real verdict.
func TestGateReconcile_SyntheticMarker(t *testing.T) {
	for _, tc := range []struct {
		desc string
		want bool
	}{
		{gateInterruptedDescription, true},
		{"review died (budget exceeded: duration…) — push again or comment the bot's command to re-run", true},
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

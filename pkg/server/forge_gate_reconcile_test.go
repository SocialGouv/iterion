package server

import (
	"context"
	"errors"
	"testing"

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
		TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
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
	for _, tc := range []struct {
		name   string
		inputs map[string]any
	}{
		{"no publish grant", map[string]any{"pr_url": "https://github.com/o/r/pull/42"}},
		{"no pr", map[string]any{forgePublishVarToken: "tok-gate"}},
		{"nothing at all", map[string]any{}},
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

// The context lives in the .bot and never reaches the server on the launch
// path. What the server does know is the one it last posted for this repo —
// learned from data, never from a bot id.
func TestGateReconcile_FallsBackToTheContextThisRepoGatesOn(t *testing.T) {
	inputs := gatingInputs()
	delete(inputs, "gate_context")

	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, inputs, gc)

	// Nothing learned yet: naming a context we cannot know would be a guess.
	_ = s.reconcileGateForRun(context.Background(), terminalEvent(runID))
	if gc.setCalls != 0 {
		t.Fatalf("invented a context for a repo it has never gated (%d writes)", gc.setCalls)
	}

	s.rememberGateContext("o/r", "revi/review")
	if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gc.setCalls != 1 || gc.last.Context != "revi/review" {
		t.Fatalf("posted %d status(es) with context %q, want 1 on revi/review", gc.setCalls, gc.last.Context)
	}
}

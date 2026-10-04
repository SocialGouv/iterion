package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// approveOrderFixture wires a webhook server with a dead gating run that owed
// revi/review on deadbeef1234, and drives the operator's force-green through
// the real /revi approve webhook flow. died is the run's terminal instant;
// updatedAt the (later) bookkeeping stamp; clockNow is what the server's
// gateClock reports during the approve.
func approveOrderFixture(t *testing.T, died, updatedAt, clockNow time.Time) (*Server, *listingGateClient, string) {
	t.Helper()
	s := newWebhookTestServer(t)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = st
	s.cfg.PublicURL = "https://iterion.test"
	s.forgePublishTokens = NewForgePublishTokenRegistry()
	s.gateSettles = newMemoryGateSettleStore(nil)
	s.gateDecisions = newMemoryGateDecisionStore(nil)
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{
		ID: "conn-app", TenantID: "t1", Provider: forge.ProviderGitHub,
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef1234"}}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) {
		return &stubCommenter{}, nil
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "authorized", nil
	}

	// The dead run, owing revi/review on the head the operator force-greens.
	// FinishedAt is stamped EXPLICITLY: only transitionRunStatus stamps it in
	// production, so a SaveRun-built fixture without it pins the UpdatedAt
	// fallback and never exercises the FinishedAt branch.
	const runID = "run-dead"
	inputs := map[string]any{
		"pr_url":             "https://github.com/acme/widgets/pull/7",
		forgePublishVarToken: "tok-gate",
		"gate_context":       "revi/review",
		"head_sha":           "deadbeef1234",
	}
	if _, err := st.CreateRun(context.Background(), runID, "review_pr", inputs); err != nil {
		t.Fatal(err)
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusCancelled
	run.FinishedAt = &died
	run.UpdatedAt = updatedAt
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	registerPublishToken(t, s, "tok-gate", ForgePublishGrant{
		TeamID: "t1", ConnectionID: "conn-app", Repo: "acme/widgets", Bot: "review-pr",
	})

	// The operator force-greens, with the server clock at clockNow.
	s.gateClock = func() time.Time { return clockNow }
	cfg, pt := ghConfig(t, s)
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":556,"body":"/revi approve false positive","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-556"},"sender":{"login":"maintainer-jane"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("approve: code=%d body=%s", w.Code, w.Body.String())
	}
	if gc.setCalls != 1 || gc.last.State != forge.CommitStateSuccess {
		t.Fatalf("the force-green did not land (calls=%d last=%q)", gc.setCalls, gc.last.State)
	}

	// The reconciler's stale read: the dead run's own pending claim.
	gc.statuses = []forge.CommitStatus{{
		State: forge.CommitStatePending, Context: "revi/review",
		Description: gateInFlightDescription,
		TargetURL:   gateRunURL(s.cfg.PublicURL, runID),
	}}
	return s, gc, runID
}

func assertNoFailurePainted(t *testing.T, s *Server, gc *listingGateClient, runID string) {
	t.Helper()
	if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, st := range gc.posted {
		if st.State == forge.CommitStateFailure {
			t.Fatalf("the reconciler painted %q over the operator's force-green — the approve never claimed the verdict-order authority (#1590)", st.Description)
		}
	}
}

// #1590, S2-HIGH: the manual /revi approve writes a success verdict on the
// gate context. Until it crosses the verdict-order authority like every other
// writer, a reconciler's synthetic failure — anchored at the run's death,
// BEFORE the operator clicked — claims the check unopposed and paints
// "review died" over the force-green.
//
// Mutations that redden this test: an approve write that does not claim the
// decision store first; or the reconciler anchoring at UpdatedAt instead of
// FinishedAt (the fixture stamps FinishedAt an hour BEFORE the UpdatedAt
// bookkeeping bump, and the approve lands between them — only the true
// terminal instant loses to it).
func TestReviewApprove_ReconcilerNeverPaintsOverAForceGreen(t *testing.T) {
	died := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	s, gc, runID := approveOrderFixture(t, died, died.Add(30*time.Minute), died.Add(15*time.Minute))
	assertNoFailurePainted(t, s, gc, runID)
}

// R2-LOW-MED (skew): FinishedAt is stamped from the RUNNER's clock, the
// approve decides on the server's. A stamp five minutes AHEAD makes the
// reconciler's anchor "newer" than an approve clicked after the true death —
// unless the anchor is clamped to what this server's clock can vouch for,
// and the resulting same-millisecond tie breaks toward the incumbent (the
// approve, which claimed first).
//
// Mutations that redden this test: no gateNow clamp on the anchor; or a
// claim granted on a timestamp tie.
func TestReviewApprove_RunnerClockSkewCannotBeatAnApprove(t *testing.T) {
	clock := time.Now().Truncate(time.Millisecond)
	s, gc, runID := approveOrderFixture(t, clock.Add(5*time.Minute), clock.Add(5*time.Minute), clock)
	assertNoFailurePainted(t, s, gc, runID)
}

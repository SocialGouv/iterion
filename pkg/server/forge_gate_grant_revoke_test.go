package server

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// A gating run keeps its publish grant for the whole sweep horizon, because a
// repair may still owe a verdict on its behalf. That is eight days of a
// forge-write bearer held by an agent that reads untrusted pull-request content
// and can post a review AND a commit status — including a green one on the
// required check.
//
// So the grant is given back the moment it is PROVABLY without a reader, and
// "the run ended" is not that moment: the reconciler may still have to speak
// for it. The moment is "the verdict this run owed is on the head".
//
// The distinction is not cosmetic. A repo's gate context is deliberately shared
// between the bots that gate it, so a verdict posted by ANOTHER run says
// nothing about whether THIS one still has something to publish — revoking on
// it would take the grant from a run on its way to its own publish step.
func TestTheGrantIsGivenBackOnlyOnThisRunsOwnVerdict(t *testing.T) {
	verdict := func(targetURL string) forge.CommitStatus {
		return forge.CommitStatus{
			Context:     "iterion/review",
			State:       forge.CommitStateSuccess,
			Description: "no blocking findings (≥high); 0 total",
			TargetURL:   targetURL,
		}
	}

	for _, tc := range []struct {
		name        string
		targetURL   string
		wantRevoked bool
	}{
		{
			name:        "its own verdict — nothing left to publish",
			targetURL:   "https://iterion.test/runs/run-gating",
			wantRevoked: true,
		},
		{
			name:        "another run's verdict on the shared context",
			targetURL:   "https://iterion.test/runs/some-other-bot",
			wantRevoked: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gc := &listingGateClient{
				fakeGateClient: fakeGateClient{headSHA: "deadbeef"},
				statuses:       []forge.CommitStatus{verdict(tc.targetURL)},
			}
			s, runID := gateReconcileFixture(t, gatingInputs(), gc)

			if _, ok := s.forgePublishTokens.lookup("tok-gate"); !ok {
				t.Fatal("fixture is wrong: the grant must exist before the reconcile")
			}
			if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if gc.setCalls != 0 {
				t.Fatalf("posted %d statuses over a real verdict, want 0", gc.setCalls)
			}

			_, stillThere := s.forgePublishTokens.lookup("tok-gate")
			switch {
			case tc.wantRevoked && stillThere:
				t.Error("the grant outlived the verdict it existed to post — a forge-write bearer kept for days past its use")
			case !tc.wantRevoked && !stillThere:
				t.Error("revoked on another run's verdict: a run still on its way to publishing just lost the credential it needs")
			}
		})
	}
}

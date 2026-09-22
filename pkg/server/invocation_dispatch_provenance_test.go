package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// A board-mode command with a cloud dispatcher RETURNS inside
// dispatchInvocation: the card is the launch, so the launch tail — and with
// it the lane-disjointness gate and the commit pin — is never reached. The
// card itself carries only BotArgs, which processBoardCard lifts into a
// LaunchSpec that hard-codes the trusted default.
//
// So a launch that knows something about its code and cannot carry it must be
// REFUSED there, not laundered into one that claims to know nothing.
func TestDispatchInvocation_RefusesProvenanceTheBoardCardCannotCarry(t *testing.T) {
	newServer := func(t *testing.T) *Server {
		t.Helper()
		s := newWebhookTestServer(t)
		s.orgUsage = orgusage.NewMemoryCounter()
		// Non-nil so the coordinator branch is taken. The guard is that
		// branch's FIRST statement, so neither value is dereferenced.
		s.cfg.CloudBoardFor = func(string) native.BoardStore { return nil }
		s.cfg.CloudBoardCoordinator = &boardmongo.Coordinator{}
		return s
	}
	cfg := webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub}
	meta := webhookEventMeta{ProjectPath: "o/r", SubjectID: "c:1", Kind: "issue_comment"}
	ctx := func() context.Context {
		return auth.WithIdentity(context.Background(), auth.Identity{UserID: "u", TeamID: "t1"})
	}

	dispatch := func(t *testing.T, s *Server, mode string, prov launchProvenance, idem string) (*httptest.ResponseRecorder, int) {
		t.Helper()
		launched := 0
		s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			launched++
			return "run-1", nil
		}
		rec := httptest.NewRecorder()
		route := webhooks.CommandRoute{BotID: "review-pr", Mode: mode}
		s.dispatchInvocation(ctx(), rec, nil, cfg, meta, idem, route, map[string]string{}, "https://github.com/o/r.git", "main", "hash", "1.2.3.4", prov)
		return rec, launched
	}

	t.Run("a fork-trust provenance on the board branch is refused", func(t *testing.T) {
		s := newServer(t)
		rec, launched := dispatch(t, s, string(bundle.ExecutionBoard), launchProvenance{Trust: store.RunTrustFork}, "idem-board-fork")
		if launched != 0 {
			t.Fatalf("launched %d run(s), want 0", launched)
		}
		if !strings.Contains(rec.Body.String(), webhooks.StatusFiltered) {
			t.Fatalf("body = %s, want a filtered status", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "cannot carry this launch's provenance") {
			t.Fatalf("body = %s, want the refusal to name why the card cannot hold it", rec.Body.String())
		}
	})

	t.Run("a pinned commit on the board branch is refused too", func(t *testing.T) {
		s := newServer(t)
		_, launched := dispatch(t, s, string(bundle.ExecutionBoard), launchProvenance{ExpectedSHA: "deadbeef"}, "idem-board-pin")
		if launched != 0 {
			t.Fatalf("launched %d run(s), want 0 — a pin the card drops is a pin nobody checks", launched)
		}
	})

	// The half that matters just as much: this refusal must NOT close the
	// path the fork review lane needs. A DIRECT route carries its provenance
	// all the way to the launch tail, which is where it is enforced — so it
	// must go through, not be caught by this guard.
	t.Run("a direct route carries the same provenance through", func(t *testing.T) {
		s := newServer(t)
		// The tail's own disjointness gate wants a fork-lane config for a
		// fork target; give it one, so what is measured here is this guard
		// and not that one.
		forkCfg := cfg
		forkCfg.ForkLane = true
		launched := 0
		s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			launched++
			return "run-1", nil
		}
		rec := httptest.NewRecorder()
		route := webhooks.CommandRoute{BotID: "review-pr", Mode: string(bundle.ExecutionDirect)}
		s.dispatchInvocation(ctx(), rec, nil, forkCfg, meta, "idem-direct-fork", route, map[string]string{},
			"https://github.com/o/r.git", "refs/pull/7/head", "hash", "1.2.3.4",
			launchProvenance{Trust: store.RunTrustFork, ExpectedSHA: "deadbeef"})
		if launched != 1 {
			t.Fatalf("a direct route with fork provenance launched %d run(s), want 1 — refusing it would close the very path the fork review lane needs (body: %s)", launched, rec.Body.String())
		}
	})
}

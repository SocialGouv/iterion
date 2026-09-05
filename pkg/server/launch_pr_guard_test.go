package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

type launchPullClient struct {
	pr  forge.PullRef
	err error
}

func (c launchPullClient) GetPullRequest(context.Context, string, int) (forge.PullRef, error) {
	return c.pr, c.err
}
func (c launchPullClient) SetCommitStatus(context.Context, string, string, forge.CommitStatus) error {
	panic("launch must not publish a status")
}

func TestPRLaunchRejectsUnprovenHead(t *testing.T) {
	for _, tc := range []struct {
		name, head string
		err        error
	}{
		{name: "fork", head: "contributor/r"},
		{name: "withheld"},
		{name: "forge-error", err: errors.New("forge unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newForgePublishTestServer(t)
			s.cfg.PublicURL = "https://iterion.example"
			s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
				return launchPullClient{pr: forge.PullRef{State: "open", HeadRepoFullName: tc.head, SourceBranch: "main"}, err: tc.err}, nil
			}
			vars := map[string]string{"pr_url": "https://github.com/o/r/pull/7"}
			out, err := s.applyPRLaunchContext(context.Background(), "team1", "", "fixer", vars, nil)
			if err == nil {
				t.Fatal("unverified PR launch was admitted")
			}
			if out[forgePublishVarToken] != "" {
				t.Fatal("unverified PR received a launch grant")
			}
		})
	}
}

func allowSameRepoLaunch(s *Server) {
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return launchPullClient{pr: forge.PullRef{State: "open", HeadRepoFullName: "o/r", SourceBranch: "topic"}}, nil
	}
}

func TestHTTPPRLaunchRejectsForkBeforeCreatingRun(t *testing.T) {
	s := newQueueOutageHTTPTestServer(t)
	installQueueOutageTestPublisher(t, s, errors.New("launch must not reach the publisher"))
	s.forgeConnections = forge.NewMemoryConnectionStore()
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "conn1", TenantID: "team1", Provider: forge.ProviderGitHub,
	}); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		lookups++
		return launchPullClient{pr: forge.PullRef{HeadRepoFullName: "contributor/r", SourceBranch: "topic"}}, nil
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{
		"file_path":"guard.bot", "source":"workflow guard:\n  entry: done\n",
		"run_id":"fork-must-not-launch", "vars":{"pr_url":"https://github.com/o/r/pull/7"}
	}`))
	req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{TeamID: "team1"}))
	rec := httptest.NewRecorder()
	s.handleLaunchRun(rec, req)
	if rec.Code != http.StatusUnprocessableEntity || lookups != 1 || !strings.Contains(rec.Body.String(), "fork") {
		t.Fatalf("status=%d lookups=%d body=%s", rec.Code, lookups, rec.Body.String())
	}
	if _, err := s.runs.RunStore().LoadRun(context.Background(), "fork-must-not-launch"); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("fork launch created a run: %v", err)
	}
}

// The guard is TEAM-SCOPED, and this is the regression that scoping exists
// for. A local studio runs with DisableAuth, whose synthesized identity has an
// EMPTY TeamID, and never wires ForgeConnections — so an unconditional guard
// resolves no connection and 422s EVERY local `--var pr_url=…` launch (Revi,
// Billy, any PR-scoped bot). Nothing is closed by refusing them: a
// repo-targeted launch, the only one that pairs <base>.CloneURL with the PR's
// head branch, is refused outside cloud mode, so a no-team launch always
// reviews this server's own already-open checkout.
func TestHTTPPRLaunchWithoutTeamSkipsTheForgeLookup(t *testing.T) {
	s := newQueueOutageHTTPTestServer(t)
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		t.Error("a no-team launch must not reach the forge")
		return nil, errors.New("unreachable")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{
		"file_path":"guard.bot", "source":"workflow guard:\n  entry: done\n",
		"run_id":"local-pr-launch", "vars":{"pr_url":"https://github.com/o/r/pull/7"}
	}`))
	// Exactly what requireAuth synthesizes under DisableAuth: an identity with
	// no team. Not "no identity at all" — that shape never reaches the handler.
	req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{UserID: "dev", IsSuperAdmin: true}))
	rec := httptest.NewRecorder()
	s.handleLaunchRun(rec, req)
	if rec.Code/100 != 2 {
		t.Fatalf("local pr_url launch refused: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := s.runs.RunStore().LoadRun(context.Background(), "local-pr-launch"); err != nil {
		t.Fatalf("local pr_url launch created no run: %v", err)
	}
}

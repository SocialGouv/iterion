package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// launchPRContextServer is a launch surface with the forge stores the
// PR-launch composition reads, and one connection covering github.com owned
// by `connTeam`.
func launchPRContextServer(t *testing.T, connTeam string) *Server {
	t.Helper()
	s := newQueueOutageHTTPTestServer(t)
	s.cfg.PublicURL = "https://iterion.test"
	s.forgeConnections = forge.NewMemoryConnectionStore()
	s.forgePublishTokens = NewForgePublishTokenRegistry()
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "conn1", TenantID: connTeam, Provider: forge.ProviderGitHub, Status: forge.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func launchAsTeam(t *testing.T, s *Server, teamID string, vars map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"file_path": "pr.bot",
		"source":    "workflow pr:\n  entry: done\n",
		"vars":      vars,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{UserID: "u1", TeamID: teamID}))
	rec := httptest.NewRecorder()
	s.handleLaunchRun(rec, req)
	return rec
}

// A launch that pins a forge publish grant minted for ANOTHER team is
// inadmissible: it can only ever be refused, and no retry changes that. Told
// 502, the studio shows a forge outage and the operator retries a request the
// server will refuse again — and the tenant crossing, which is the whole
// point of the check, never reaches them.
func TestLaunchPinningAnotherTeamsGrantIs422(t *testing.T) {
	s := launchPRContextServer(t, "team1")
	registerPublishToken(t, s, "tok-team1", ForgePublishGrant{
		TeamID: "team1", ConnectionID: "conn1", Repo: "o/r", Bot: "review-pr",
	})

	rec := launchAsTeam(t, s, "team-b", map[string]string{
		"pr_url":             "https://github.com/o/r/pull/42",
		forgePublishVarToken: "tok-team1",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 — a tenant-crossing pin is an inadmissible request, not an upstream forge failure; body=%s",
			rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "team") {
		t.Errorf("body = %s, want the refusal reason (which team the grant belongs to)", body)
	}
	if strings.Contains(rec.Body.String(), "tok-team1") {
		t.Errorf("the refusal leaks the grant token: %s", rec.Body.String())
	}
}

// forkGuardErrClient makes the fork guard's PR read fail the way an
// unreachable forge does.
type forkGuardErrClient struct{ fakeGateClient }

func (c *forkGuardErrClient) GetPullRequest(_ context.Context, _ string, _ int) (forge.PullRef, error) {
	return forge.PullRef{}, errors.New("dial tcp: connection refused")
}

// The 502 arm is not removed, only narrowed: a forge that could not be asked
// is still an upstream failure.
func TestLaunchWhoseForkGuardCannotReachTheForgeIs502(t *testing.T) {
	s := launchPRContextServer(t, "team-b")
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return &forkGuardErrClient{}, nil
	}
	rec := launchAsTeam(t, s, "team-b", map[string]string{"pr_url": "https://github.com/o/r/pull/42"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 — the forge could not be asked; body=%s", rec.Code, rec.Body.String())
	}
}

// A fork PR the guard PROVED is a fork stays 422, unchanged.
func TestLaunchOnAForkPRIs422(t *testing.T) {
	s := launchPRContextServer(t, "team-b")
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return &fakeGateClient{headSHA: "abc", headRepo: "mallory/r"}, nil
	}
	rec := launchAsTeam(t, s, "team-b", map[string]string{"pr_url": "https://github.com/o/r/pull/42"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// A publish grant the server could not mint is the server's OWN capacity —
// the one row that stays 503 (retriable as-is).
func TestLaunchWhoseGrantCannotBeMintedIs503(t *testing.T) {
	s := launchPRContextServer(t, "team-b")
	s.forgePublishTokens = &failingPublishTokenStore{
		ForgePublishTokenStore: NewForgePublishTokenRegistry(),
		err:                    errors.New("forge publish token registry full"),
	}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return &fakeGateClient{headSHA: "abc"}, nil // same-repo: the guard passes
	}
	rec := launchAsTeam(t, s, "team-b", map[string]string{"pr_url": "https://github.com/o/r/pull/42"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

// A connection whose provider cannot read pull requests can never prove
// same-repo: no retry helps, so it is not an upstream failure either.
func TestLaunchOnAConnectionThatCannotReadPullRequestsIs422(t *testing.T) {
	s := launchPRContextServer(t, "team-b")
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return nil, nil
	}
	rec := launchAsTeam(t, s, "team-b", map[string]string{"pr_url": "https://github.com/o/r/pull/42"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

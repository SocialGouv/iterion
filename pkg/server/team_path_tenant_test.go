package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A team-scoped write must land in the team named in the PATH, not in the
// tenant the caller's token happens to be pinned to (#997).
//
// The two are routinely different: canManageTeam admits a super-admin (and
// an org admin over any team of their org) on a team that is not their
// active one — that is the documented way an SRE wires another team's bot
// credential. Before the fix the row was stamped with the caller's tenant,
// so it was invisible from the target team AND from the caller's own list
// (which filters on the path team's scope), and the target team's runs
// resolved no credential at all. Nothing errored.
//
// Measured in production on 2026-09-08 while wiring the Jira credential of
// `demat-amiante`: see docs/bot-runs/review-pr.md.
func newTenantScopeTestServer(t *testing.T) *Server {
	t.Helper()
	s := newOrgTestServer(t)
	s.genericSecrets = secrets.NewMemoryGenericSecretStore()
	s.botBindings = secrets.NewMemoryBotSecretBindingStore()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sealer, err := secrets.NewAESGCMSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	s.sealer = sealer
	ctx := context.Background()
	for _, id := range []string{"team-caller", "team-target"} {
		if _, err := s.authStore().CreateTeam(ctx, identity.Team{
			ID: id, Name: id, Slug: id, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// callerCtx is a super-admin whose ACTIVE team is team-caller — the shape
// `stampAuthedContext` produces for a real request.
func callerCtx() context.Context {
	ctx := auth.WithIdentity(context.Background(), auth.Identity{
		UserID: "admin", IsSuperAdmin: true, TeamID: "team-caller",
	})
	return store.WithIdentity(ctx, "team-caller", "admin")
}

// targetRunCtx is what a RUN of the target team reads with — the context
// that must be able to see the credential that was wired for it.
func targetRunCtx() context.Context {
	return store.WithTenant(context.Background(), "team-target")
}

func TestTeamSecretWrittenFromAnotherActiveTeamIsVisibleToThatTeam(t *testing.T) {
	s := newTenantScopeTestServer(t)

	w := httptest.NewRecorder()
	req := bindReq(callerCtx(), "POST", "/api/teams/team-target/secrets",
		`{"name":"jira_token","secret":"s3cr3t-value"}`, "team-target", "", "")
	s.handleCreateTeamSecret(w, req)
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("bad create body %s (%v)", w.Body.String(), err)
	}

	// The run of the team the secret was created FOR must find it.
	if _, err := s.genericSecrets.Get(targetRunCtx(), created.ID); err != nil {
		t.Fatalf("secret created for team-target is invisible to its own runs: %v", err)
	}

	// And the API list for that team must show it.
	w = httptest.NewRecorder()
	s.handleListTeamSecrets(w, bindReq(callerCtx(), "GET", "/api/teams/team-target/secrets", "", "team-target", "", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s", w.Code, w.Body.String())
	}
	var listed struct {
		Secrets []struct {
			ID string `json:"id"`
		} `json:"secrets"`
	}
	json.Unmarshal(w.Body.Bytes(), &listed)
	found := false
	for _, sec := range listed.Secrets {
		if sec.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("secret absent from the target team's list: %s", w.Body.String())
	}
}

func TestBotBindingWrittenFromAnotherActiveTeamIsResolvableByThatTeam(t *testing.T) {
	s := newTenantScopeTestServer(t)

	// The secret the binding will reference, already correctly scoped.
	if err := s.genericSecrets.Create(targetRunCtx(), secrets.GenericSecret{
		ID: "sec-1", TenantID: "team-target", ScopeTeamID: "team-target",
		Name: "jira_token", SealedSecret: []byte("x"), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleCreateBotBinding(w, bindReq(callerCtx(), "POST",
		"/api/teams/team-target/bots/review-pr/bindings",
		`{"secret_id":"sec-1","secret_name_for_workflow":"tracker_token"}`,
		"team-target", "review-pr", ""))
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("create binding: code=%d body=%s", w.Code, w.Body.String())
	}

	// The publisher resolves bindings under the RUN's tenant.
	list, err := s.botBindings.ListByTenantBot(targetRunCtx(), "team-target", "review-pr")
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("binding created for team-target is invisible to its own runs (%d found) — the credential silently never reaches the bot", len(list))
	}
}

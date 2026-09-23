package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Mutating a team-scoped credential is the same right as creating one.
//
// Creation goes through canManageTeam, which carries the org arm; revocation
// went through canMutateScopedRecord, which did not. An org admin could
// therefore plant a credential in any team of their org and never take it
// back — strictly worse than not being able to plant it, since the key funds
// runs and its secret is write-only from that point on.

const teamKeyBody = `{"provider":"anthropic","name":"team key","secret":"sk-ant-api03-team-scoped-key"}`

// orgAdminSessionCtx is the shared orgAdminCtx() plus the claims a real
// access token carries (pkg/auth/jwt.go). The shared helper leaves OrgID and
// OrgRole empty, which makes "the CALLER's org" the empty string — so a
// predicate that read the caller's claimed org instead of the TEAM's would
// fail closed for every fixture and go unnoticed. Kept local: canViewOrg
// short-circuits on these two claims, so handing them to every test using the
// shared helper would let unrelated assertions pass through the claim instead
// of the store.
func orgAdminSessionCtx() context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{
		UserID: "orgadmin", OrgID: "o1", OrgRole: identity.OrgRoleAdmin,
	})
}

// recordReq names the team in {id} — the tenant the store reads under — and
// the record in its own path segment.
func recordReq(ctx context.Context, method, path, body, teamID, seg, recordID string) *http.Request {
	r := orgReq(ctx, method, path, body, teamID)
	r.SetPathValue(seg, recordID)
	return r
}

func seedTeamApiKey(t *testing.T, s *Server, ctx context.Context, teamID string) string {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleCreateTeamApiKey(w, orgReq(ctx, "POST", "/api/teams/"+teamID+"/api-keys", teamKeyBody, teamID))
	if w.Code != http.StatusOK {
		t.Fatalf("seed key on %s: code=%d body=%s", teamID, w.Code, w.Body.String())
	}
	keys, err := s.apiKeys.ListByTeam(withTenantCtx(teamID), teamID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("seed key on %s: %d keys, want 1", teamID, len(keys))
	}
	return keys[0].ID
}

// The asymmetry, end to end: the org admin creates the key, then revokes it.
func TestScopedRecord_OrgAdminRevokesTheTeamKeyTheyCouldCreate(t *testing.T) {
	s := newOrgCredsTestServer(t)
	keyID := seedTeamApiKey(t, s, orgAdminCtx(), "t1")

	w := httptest.NewRecorder()
	s.handleUpdateApiKey(w, recordReq(orgAdminCtx(), "PATCH", "/api/teams/t1/api-keys/"+keyID, `{"name":"rotated"}`, "t1", "key_id", keyID))
	if w.Code != http.StatusOK {
		t.Fatalf("update as org admin: code=%d body=%s, want 200", w.Code, w.Body.String())
	}
	got, err := s.apiKeys.Get(withTenantCtx("t1"), keyID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "rotated" {
		t.Fatalf("key name = %q, want rotated — the 200 did not write", got.Name)
	}

	w = httptest.NewRecorder()
	s.handleDeleteApiKey(w, recordReq(orgAdminCtx(), "DELETE", "/api/teams/t1/api-keys/"+keyID, "", "t1", "key_id", keyID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete as org admin: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	if _, err := s.apiKeys.Get(withTenantCtx("t1"), keyID); err == nil {
		t.Fatal("the key survived a 204 delete")
	}
}

// The same rule on the other record that shares the predicate. A forge token
// an org admin can install and not rotate is the same trap as a BYOK key.
func TestScopedRecord_OrgAdminRevokesTheTeamSecretTheyCouldCreate(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.genericSecrets = secrets.NewMemoryGenericSecretStore()

	w := httptest.NewRecorder()
	s.handleCreateTeamSecret(w, orgReq(orgAdminCtx(), "POST", "/api/teams/t1/secrets", `{"name":"org_gitlab","secret":"glpat-xxxxxxxx"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("create secret as org admin: code=%d body=%s", w.Code, w.Body.String())
	}
	recs, err := s.genericSecrets.ListByTeam(withTenantCtx("t1"), "t1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("%d secrets, want 1", len(recs))
	}
	secID := recs[0].ID

	w = httptest.NewRecorder()
	s.handleUpdateGenericSecret(w, recordReq(orgAdminCtx(), "PATCH", "/api/teams/t1/secrets/"+secID, `{"name":"org_gitlab_v2"}`, "t1", "secret_id", secID))
	if w.Code != http.StatusOK {
		t.Fatalf("update secret as org admin: code=%d body=%s, want 200", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.handleDeleteGenericSecret(w, recordReq(orgAdminCtx(), "DELETE", "/api/teams/t1/secrets/"+secID, "", "t1", "secret_id", secID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete secret as org admin: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	if _, err := s.genericSecrets.Get(withTenantCtx("t1"), secID); err == nil {
		t.Fatal("the secret survived a 204 delete")
	}
}

// The arm is the team's OWN org, not any org. t9 lives in o2; o1's admin is a
// stranger to it and stays one — this is the mutant that reddens if the fix
// reaches for canManageOrg on the caller's org instead of the team's.
func TestScopedRecord_OrgAdminOfAnotherOrgStaysRefused(t *testing.T) {
	s := newOrgCredsTestServer(t)
	keyID := seedTeamApiKey(t, s, superAdminCtx(), "t9")

	w := httptest.NewRecorder()
	s.handleUpdateApiKey(w, recordReq(orgAdminSessionCtx(), "PATCH", "/api/teams/t9/api-keys/"+keyID, `{"name":"stolen"}`, "t9", "key_id", keyID))
	if w.Code != http.StatusForbidden {
		t.Fatalf("update a foreign org's team key: code=%d body=%s, want 403", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.handleDeleteApiKey(w, recordReq(orgAdminSessionCtx(), "DELETE", "/api/teams/t9/api-keys/"+keyID, "", "t9", "key_id", keyID))
	if w.Code != http.StatusForbidden {
		t.Fatalf("delete a foreign org's team key: code=%d body=%s, want 403", w.Code, w.Body.String())
	}
	if _, err := s.apiKeys.Get(withTenantCtx("t9"), keyID); err != nil {
		t.Fatal("the refused delete removed the key anyway")
	}
}

// The FLOOR of the widening. Adding an arm must not move the one that was
// already there: a team admin who is not an org admin keeps revoking their
// own team's credentials. Reddens on a fix that swaps the membership read for
// orgAdminOfTeam instead of widening to canManageTeam.
func TestScopedRecord_TeamAdminStillRevokesTheirOwnTeamKey(t *testing.T) {
	s := newOrgCredsTestServer(t)
	keyID := seedTeamApiKey(t, s, teamAdminCtx(), "t1")

	w := httptest.NewRecorder()
	s.handleUpdateApiKey(w, recordReq(teamAdminCtx(), "PATCH", "/api/teams/t1/api-keys/"+keyID, `{"name":"rotated"}`, "t1", "key_id", keyID))
	if w.Code != http.StatusOK {
		t.Fatalf("update as team admin: code=%d body=%s, want 200", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.handleDeleteApiKey(w, recordReq(teamAdminCtx(), "DELETE", "/api/teams/t1/api-keys/"+keyID, "", "t1", "key_id", keyID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete as team admin: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
}

// The CEILING of the widening, and the reason it needs one: rotating or
// deleting a team credential is an administration act, not a membership
// perk. Reddens on a fix that reaches for canViewTeam — under which every
// member and viewer of the team could revoke its BYOK key and its forge
// tokens.
func TestScopedRecord_PlainTeamMemberCannotRevoke(t *testing.T) {
	s := newOrgCredsTestServer(t)
	if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{
		UserID: "plainmember", TeamID: "t1", Role: identity.RoleMember,
	}); err != nil {
		t.Fatal(err)
	}
	memberCtx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "plainmember", TeamID: "t1"})
	keyID := seedTeamApiKey(t, s, teamAdminCtx(), "t1")

	w := httptest.NewRecorder()
	s.handleUpdateApiKey(w, recordReq(memberCtx, "PATCH", "/api/teams/t1/api-keys/"+keyID, `{"name":"rotated"}`, "t1", "key_id", keyID))
	if w.Code != http.StatusForbidden {
		t.Fatalf("update as plain member: code=%d body=%s, want 403", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.handleDeleteApiKey(w, recordReq(memberCtx, "DELETE", "/api/teams/t1/api-keys/"+keyID, "", "t1", "key_id", keyID))
	if w.Code != http.StatusForbidden {
		t.Fatalf("delete as plain member: code=%d body=%s, want 403", w.Code, w.Body.String())
	}
	if _, err := s.apiKeys.Get(withTenantCtx("t1"), keyID); err != nil {
		t.Fatal("the refused delete removed the key anyway")
	}
}

// The rule follows the RECORD's scope, not the path. PATCH/DELETE on
// /api/me/api-keys/{key_id} is the SAME handler as the team route with no
// {id} segment, so the store tenant stays the caller's active team and a
// TEAM-scoped row of that team is reachable there — and governed by the team
// rule, org arm included. Documented in docs/cloud-rest-api.md; this pins it,
// because the sibling GET on the same prefix lists user-scoped keys only and
// makes the asymmetry invisible.
func TestScopedRecord_MeRouteFollowsTheRecordScopeNotThePath(t *testing.T) {
	s := newOrgCredsTestServer(t)
	keyID := seedTeamApiKey(t, s, teamAdminCtx(), "t1")

	// The /api/me shape: no {id} to re-scope from, tenant stamped by
	// requireAuth from the caller's active team.
	meCtx := store.WithTenant(orgAdminSessionCtx(), "t1")
	r := httptest.NewRequest("DELETE", "/api/me/api-keys/"+keyID, nil).WithContext(meCtx)
	r.SetPathValue("key_id", keyID)

	w := httptest.NewRecorder()
	s.handleDeleteApiKey(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("org admin deleting a team key through /api/me: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
}

// A user-scoped key is personal. The org arm must not reach into it: an org
// admin is not the owner of a member's own credential, and the record carries
// no team-wide meaning to administer.
func TestScopedRecord_OrgAdminCannotTouchAMembersOwnKey(t *testing.T) {
	s := newOrgCredsTestServer(t)

	// handleCreateMyApiKey scopes on the caller's ACTIVE team, so the
	// identity carries one.
	ownerCtx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "teamadmin", TeamID: "t1"})
	w := httptest.NewRecorder()
	s.handleCreateMyApiKey(w, orgReq(ownerCtx, "POST", "/api/me/api-keys", teamKeyBody, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("create own key: code=%d body=%s", w.Code, w.Body.String())
	}
	keys, err := s.apiKeys.ListByUser(withTenantCtx("t1"), "t1", "teamadmin")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("%d user-scoped keys, want 1", len(keys))
	}
	keyID := keys[0].ID

	w = httptest.NewRecorder()
	s.handleDeleteApiKey(w, recordReq(orgAdminCtx(), "DELETE", "/api/teams/t1/api-keys/"+keyID, "", "t1", "key_id", keyID))
	if w.Code != http.StatusForbidden {
		t.Fatalf("delete a member's own key as org admin: code=%d body=%s, want 403", w.Code, w.Body.String())
	}
	if _, err := s.apiKeys.Get(withTenantCtx("t1"), keyID); err != nil {
		t.Fatal("the refused delete removed the member's key anyway")
	}
}

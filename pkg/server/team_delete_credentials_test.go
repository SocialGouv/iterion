package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The #1687 class: a credential that can be installed must be countable by
// the team emptiness gate, whatever its scope, and a revoke that finds
// nothing must not answer a success receipt.

// The emptiness gate's witness, keys arm: a team holding EXACTLY ONE
// user-scoped key is not empty. ListByTeam with no requesting user reads
// only team-wide rows (its contract — the per-user resolution view), so a
// probe through it counts zero here and the delete lands with a sealed
// credential left behind in a tenant nothing can reach; the mutant that
// restores it reddens this test.
func TestTeamLifecycle_deleteRefusesATeamWithAUserScopedKey(t *testing.T) {
	s := newOrgCredsTestServer(t)
	if err := s.apiKeys.Create(withTenantCtx("t2"), secrets.ApiKey{
		ID: "key-personal", ScopeTeamID: "t2", ScopeUserID: "someone",
		Provider: secrets.ProviderAnthropic, Name: "personal", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleDeleteTeam(w, teamReq(orgAdminCtx(), "DELETE", "/api/teams/t2", "", "t2", ""))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete a team holding a user-scoped key: code=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "api key") {
		t.Errorf("the refusal does not name the key: %s", body)
	}
	if _, err := s.authStore().GetTeam(context.Background(), "t2"); err != nil {
		t.Fatalf("the team was deleted despite the refusal: %v", err)
	}
}

// The secrets arm of the same gate: a team holding exactly one user-scoped
// named secret is not empty either.
func TestTeamLifecycle_deleteRefusesATeamWithAUserScopedSecret(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.genericSecrets = secrets.NewMemoryGenericSecretStore()
	if err := s.genericSecrets.Create(withTenantCtx("t2"), secrets.GenericSecret{
		ID: "sec-personal", ScopeTeamID: "t2", ScopeUserID: "someone",
		Name: "PERSONAL_TOKEN", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleDeleteTeam(w, teamReq(orgAdminCtx(), "DELETE", "/api/teams/t2", "", "t2", ""))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete a team holding a user-scoped secret: code=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "secret") {
		t.Errorf("the refusal does not name the secret: %s", body)
	}
}

// A team whose credentials are gone still deletes — the gate counts, it
// does not block emptiness itself.
func TestTeamLifecycle_deleteAnEmptyTeamCountsNoCredential(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.genericSecrets = secrets.NewMemoryGenericSecretStore()

	w := httptest.NewRecorder()
	s.handleDeleteTeam(w, teamReq(orgAdminCtx(), "DELETE", "/api/teams/t2", "", "t2", ""))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete an empty team: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
}

// deleteMyKey calls DELETE /api/me/api-keys/{keyID} as userID whose ACTIVE
// team is activeTeam — the orphaned-revoke shape: under another team's
// tenant the store filter hides the row (#1687).
func deleteMyKey(srv *Server, keyID, userID, activeTeam string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("DELETE", "/api/me/api-keys/"+keyID, nil)
	r.SetPathValue("key_id", keyID)
	rctx := auth.WithIdentity(r.Context(), auth.Identity{UserID: userID, TeamID: activeTeam})
	r = r.WithContext(store.WithTenant(rctx, activeTeam))
	w := httptest.NewRecorder()
	srv.handleDeleteApiKey(w, r)
	return w
}

// The revoke's witness: a DELETE that finds nothing — an unknown id, or a
// row the tenant filter hides — answers 404, never the 204 success receipt
// for a deletion that did not happen; and the hidden row SURVIVES (no
// delete happened, and none is claimed). Under its own tenant the row is
// the owner's to delete, and that one answers 204.
func TestDeleteApiKey_AMissIs404NotASuccessReceipt(t *testing.T) {
	srv, spy, _, _ := byokServer(t)
	ctx := store.WithTenant(context.Background(), "team-a")
	if err := spy.Create(ctx, secrets.ApiKey{
		ID: "key-personal", ScopeTeamID: "team-a", ScopeUserID: "u2",
		Provider: secrets.ProviderAnthropic, Name: "personal", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if w := deleteMyKey(srv, "no-such-key", "u2", "team-a"); w.Code != http.StatusNotFound {
		t.Fatalf("delete an unknown key: %d body=%s, want 404", w.Code, w.Body.String())
	}
	// The orphaned shape: the row exists, but under team-b's tenant the
	// filter hides it — as it is hidden from every team once team-a is gone.
	if w := deleteMyKey(srv, "key-personal", "u2", "team-b"); w.Code != http.StatusNotFound {
		t.Fatalf("delete a tenant-hidden key: %d body=%s, want 404 — never a success receipt for a no-op revoke", w.Code, w.Body.String())
	}
	if _, err := spy.Get(ctx, "key-personal"); err != nil {
		t.Fatalf("the hidden row is GONE — a no-op delete claimed success and deleted nothing is the honest pair; got %v", err)
	}
	// Under its own tenant it is the owner's to delete, and that delete
	// answers 204.
	if w := deleteMyKey(srv, "key-personal", "u2", "team-a"); w.Code != http.StatusNoContent {
		t.Fatalf("delete the own key under its tenant: %d body=%s, want 204", w.Code, w.Body.String())
	}
	if _, err := spy.Get(ctx, "key-personal"); err == nil {
		t.Fatal("the row survived its deletion")
	}
}

// The secrets surface is the same handler shape as the keys': its miss
// answers 404 too — the two surfaces change together.
func TestDeleteGenericSecret_AMissIs404NotASuccessReceipt(t *testing.T) {
	srv := &Server{genericSecrets: secrets.NewMemoryGenericSecretStore()}
	ctx := store.WithTenant(context.Background(), "team-a")
	if err := srv.genericSecrets.Create(ctx, secrets.GenericSecret{
		ID: "sec-personal", ScopeTeamID: "team-a", ScopeUserID: "u2",
		Name: "PERSONAL_TOKEN", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	del := func(secretID, userID, activeTeam string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("DELETE", "/api/me/secrets/"+secretID, nil)
		r.SetPathValue("secret_id", secretID)
		rctx := auth.WithIdentity(r.Context(), auth.Identity{UserID: userID, TeamID: activeTeam})
		r = r.WithContext(store.WithTenant(rctx, activeTeam))
		w := httptest.NewRecorder()
		srv.handleDeleteGenericSecret(w, r)
		return w
	}

	if w := del("no-such-secret", "u2", "team-a"); w.Code != http.StatusNotFound {
		t.Fatalf("delete an unknown secret: %d body=%s, want 404", w.Code, w.Body.String())
	}
	if w := del("sec-personal", "u2", "team-b"); w.Code != http.StatusNotFound {
		t.Fatalf("delete a tenant-hidden secret: %d body=%s, want 404", w.Code, w.Body.String())
	}
	if _, err := srv.genericSecrets.Get(ctx, "sec-personal"); err != nil {
		t.Fatalf("the hidden row is gone: %v", err)
	}
	if w := del("sec-personal", "u2", "team-a"); w.Code != http.StatusNoContent {
		t.Fatalf("delete the own secret under its tenant: %d body=%s, want 204", w.Code, w.Body.String())
	}
}

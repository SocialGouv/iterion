package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The org credential tier's control surface. Three things must hold, and
// each failure is a credential leak rather than a broken feature:
// only ORG admins administer it, the rows land under the reserved scope
// the publisher reads, and the audience cannot name a team of another org.

// newOrgCredsTestServer builds a server with the BYOK stores wired plus org
// o1 owning teams t1 and t2, a t1 team-admin who is NOT an org admin, and
// an org admin. A second org o2 with team t9 backs the cross-org check.
func newOrgCredsTestServer(t *testing.T) *Server {
	t.Helper()
	s := newOrgTestServer(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	sealer, err := secrets.NewAESGCMSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	s.sealer = sealer
	s.apiKeys = secrets.NewMemoryApiKeyStore()

	ctx := context.Background()
	for _, o := range []string{"o1", "o2"} {
		if _, err := s.authStore().CreateOrg(ctx, identity.Org{ID: o, Name: o, Slug: o, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tm := range []struct{ id, org string }{{"t1", "o1"}, {"t2", "o1"}, {"t9", "o2"}} {
		if _, err := s.authStore().CreateTeam(ctx, identity.Team{
			ID: tm.id, Name: tm.id, Slug: tm.id, OrgID: tm.org, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: "teamadmin", TeamID: "t1", Role: identity.RoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: "orgadmin", OrgID: "o1", Role: identity.OrgRoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// withTenantCtx reads the store the way the publisher does — under an
// explicit tenant — since the api-key store filters on the context tenant.
func withTenantCtx(tenant string) context.Context {
	return store.WithTenant(context.Background(), tenant)
}

const orgKeyBody = `{"provider":"anthropic","name":"org shared","secret":"sk-ant-api03-org-shared-key"}`

// A TEAM admin is not an org admin. Letting one create the org's shared key
// would let any team of the org mint a credential every other team can
// spend — the audience would still gate the spending, but the key itself is
// the org's to own.
func TestOrgCredentials_teamAdminCannotAdministerTheOrgKeys(t *testing.T) {
	s := newOrgCredsTestServer(t)

	w := httptest.NewRecorder()
	s.handleCreateOrgApiKey(w, orgReq(teamAdminCtx(), "POST", "/api/orgs/o1/api-keys", orgKeyBody, "o1"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("create as team admin: code=%d body=%s, want 403", w.Code, w.Body.String())
	}
}

// The org admin creates the key, and it MUST land under the reserved
// org-tier scope: the publisher reads there, so a row stamped with the raw
// org id would be invisible to every run and the org would silently fall
// through to the platform credential.
func TestOrgCredentials_keyLandsUnderTheReservedScope(t *testing.T) {
	s := newOrgCredsTestServer(t)

	w := httptest.NewRecorder()
	s.handleCreateOrgApiKey(w, orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/api-keys", orgKeyBody, "o1"))
	if w.Code != http.StatusOK {
		t.Fatalf("create as org admin: code=%d body=%s", w.Code, w.Body.String())
	}

	scope := secrets.OrgTierTenantID("o1")
	keys, err := s.apiKeys.ListByTeam(withTenantCtx(scope), scope, "")
	if err != nil {
		t.Fatalf("list under the org-tier scope: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("found %d key(s) under %q, want 1 — the row did not land where the publisher reads", len(keys), scope)
	}
	if keys[0].ScopeTeamID != scope {
		t.Errorf("ScopeTeamID = %q, want %q", keys[0].ScopeTeamID, scope)
	}
	if keys[0].ScopeUserID != "" {
		t.Errorf("ScopeUserID = %q — an org key is the org's, never a person's", keys[0].ScopeUserID)
	}

	// And it must NOT be visible as a key of any team of the org: a team
	// listing its own BYOK would otherwise show a credential it cannot
	// manage and does not own.
	teamKeys, err := s.apiKeys.ListByTeam(withTenantCtx("t1"), "t1", "teamadmin")
	if err != nil {
		t.Fatalf("list t1 keys: %v", err)
	}
	if len(teamKeys) != 0 {
		t.Errorf("team t1 sees %d org-tier key(s) as its own BYOK", len(teamKeys))
	}
}

// The audience is an authorization list. A team id from ANOTHER org must be
// refused: accepting it would lend this org's subscription across a tenant
// boundary, with nothing downstream to catch it (the publisher trusts the
// audience by design).
func TestOrgCredentials_audienceRefusesATeamOfAnotherOrg(t *testing.T) {
	s := newOrgCredsTestServer(t)

	w := httptest.NewRecorder()
	s.handleUpdateOrgCredentialAudience(w,
		orgReq(orgAdminCtx(), "PATCH", "/api/orgs/o1/credential-audience", `{"teams":["t1","t9"]}`, "o1"))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("audience with a foreign team: code=%d body=%s, want 422", w.Code, w.Body.String())
	}

	o, err := s.authStore().GetOrg(context.Background(), "o1")
	if err != nil {
		t.Fatal(err)
	}
	if len(o.CredentialAudience.Teams) != 0 {
		t.Errorf("audience = %v after a refused request — a rejected grant was partially applied",
			o.CredentialAudience.Teams)
	}
}

// The happy path: own teams are accepted, deduplicated, and readable back.
func TestOrgCredentials_audienceAcceptsOwnTeams(t *testing.T) {
	s := newOrgCredsTestServer(t)

	w := httptest.NewRecorder()
	s.handleUpdateOrgCredentialAudience(w,
		orgReq(orgAdminCtx(), "PATCH", "/api/orgs/o1/credential-audience", `{"teams":["t1","t2","t1"]}`, "o1"))
	if w.Code != http.StatusOK {
		t.Fatalf("audience update: code=%d body=%s", w.Code, w.Body.String())
	}
	var got orgCredentialAudienceView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Teams) != 2 || got.Teams[0] != "t1" || got.Teams[1] != "t2" {
		t.Fatalf("teams = %v, want [t1 t2] deduplicated", got.Teams)
	}

	o, err := s.authStore().GetOrg(context.Background(), "o1")
	if err != nil {
		t.Fatal(err)
	}
	if !o.CredentialAudience.Allows("t1") || o.CredentialAudience.Allows("t9") {
		t.Errorf("persisted audience admits the wrong set: %+v", o.CredentialAudience)
	}
}

// A team admin may READ the audience of an org they belong to (they are an
// org member) but never write it.
func TestOrgCredentials_teamAdminCannotWriteTheAudience(t *testing.T) {
	s := newOrgCredsTestServer(t)
	if err := s.authStore().UpsertOrgMembership(context.Background(), identity.OrgMembership{
		UserID: "teamadmin", OrgID: "o1", Role: identity.OrgRoleMember,
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleGetOrgCredentialAudience(w, orgReq(teamAdminCtx(), "GET", "/api/orgs/o1/credential-audience", "", "o1"))
	if w.Code != http.StatusOK {
		t.Fatalf("read as org member: code=%d body=%s, want 200", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.handleUpdateOrgCredentialAudience(w,
		orgReq(teamAdminCtx(), "PATCH", "/api/orgs/o1/credential-audience", `{"all_teams":true}`, "o1"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("write as org member: code=%d body=%s, want 403", w.Code, w.Body.String())
	}
}

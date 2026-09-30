package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #1931: /api/teams/{id}/… accepted the team's slug AND its UUID (a
// super-admin's canManageTeam passes either way) but keyed every
// team-scoped row by the RAW path spelling — the binding's tenant_id, the
// secret's scope_team, the ctx tenant the stores stamp and filter on. A
// row written via --team <slug> was then invisible to every reader
// resolving under the UUID — the publisher at launch included, which found
// 0 binding and ran the bot WITHOUT its credential — and a PATCH naming
// the other spelling failed the misleading "not a team-scoped secret in
// this org" 400. The fix canonicalizes {id} to the team UUID at route
// resolution, so writers and readers are spelling-insensitive; these are
// the witnesses.

// newTeamSlugServer is the run-scope fixture (tenant-A with slug team-a,
// u-both its admin, a real signer) plus the credential stores and their
// routes — wired by hand since routes() registers them only when the
// stores are present at construction.
func newTeamSlugServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	srv, _ := newRunScopeServer(t)
	srv.genericSecrets = secrets.NewMemoryGenericSecretStore()
	srv.botBindings = secrets.NewMemoryBotSecretBindingStore()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sealer, err := secrets.NewAESGCMSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	srv.sealer = sealer
	srv.registerGenericSecretRoutes()
	srv.registerBotBindingRoutes()
	return srv, fullStack(t, srv)
}

func teamCall(t *testing.T, ts *httptest.Server, method, path, token, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func bearer(t *testing.T, srv *Server, id auth.Identity) string {
	t.Helper()
	token, _, err := srv.signer.IssueAccess(id)
	if err != nil {
		t.Fatalf("mint bearer: %v", err)
	}
	return token
}

// A secret written through the SLUG path is one the UUID path lists, and
// the store keys it by the team's UUID alone — no slug-spelling row
// coexists. The reverse direction too: the two spellings are one team.
func TestTeamSecretWrittenBySlugIsReadByUUID(t *testing.T) {
	srv, ts := newTeamSlugServer(t)
	sre := bearer(t, srv, auth.Identity{UserID: "sre", IsSuperAdmin: true})

	code, raw := teamCall(t, ts, "POST", "/api/teams/team-a/secrets", sre, `{"name":"jira_token","secret":"s3cr3t-value"}`)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create via slug: %d %s", code, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.ID == "" {
		t.Fatalf("bad create body %s (%v)", raw, err)
	}

	// The UUID spelling lists it — before the canonicalization this list
	// was empty: the row had landed keyed by the raw slug.
	code, raw = teamCall(t, ts, "GET", "/api/teams/tenant-A/secrets", sre, "")
	if code != http.StatusOK {
		t.Fatalf("list via UUID: %d %s", code, raw)
	}
	var listed struct {
		Secrets []struct {
			ID string `json:"id"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Secrets) != 1 || listed.Secrets[0].ID != created.ID {
		t.Fatalf("a secret written via the slug is invisible under the UUID: %s", raw)
	}

	// The run of the team reads under the UUID (the publisher's ctx); the
	// row must be there, and no row may answer to the slug spelling.
	if _, err := srv.genericSecrets.Get(store.WithTenant(context.Background(), "tenant-A"), created.ID); err != nil {
		t.Fatalf("the team's runs cannot resolve the secret: %v", err)
	}
	if _, err := srv.genericSecrets.Get(store.WithTenant(context.Background(), "team-a"), created.ID); err == nil {
		t.Fatal("a row keyed by the raw slug spelling coexists with the canonical one")
	}

	// And the reverse direction: written via UUID, listed via slug.
	code, raw = teamCall(t, ts, "POST", "/api/teams/tenant-A/secrets", sre, `{"name":"tracker_token","secret":"oth3r-value"}`)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create via UUID: %d %s", code, raw)
	}
	code, raw = teamCall(t, ts, "GET", "/api/teams/team-a/secrets", sre, "")
	if code != http.StatusOK {
		t.Fatalf("list via slug: %d %s", code, raw)
	}
	var listed2 struct {
		Secrets []struct {
			Name string `json:"name"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(raw, &listed2); err != nil || len(listed2.Secrets) != 2 {
		t.Fatalf("a secret written via the UUID is invisible under the slug: %s", raw)
	}
}

// The ticket's misleading 400 — "secret_id is not a team-scoped secret in
// this org" when the secret was written under the other spelling — is
// impossible once both spellings key the same row: a binding created
// across spellings validates, and the team's runs (reading under the
// UUID) resolve it.
func TestBotBindingValidatesASecretAcrossSpellings(t *testing.T) {
	srv, ts := newTeamSlugServer(t)
	sre := bearer(t, srv, auth.Identity{UserID: "sre", IsSuperAdmin: true})

	// Secret via the slug, binding via the UUID.
	code, raw := teamCall(t, ts, "POST", "/api/teams/team-a/secrets", sre, `{"name":"jira_token","secret":"s3cr3t-value"}`)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create secret: %d %s", code, raw)
	}
	var sec struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &sec); err != nil {
		t.Fatal(err)
	}
	body := `{"secret_id":"` + sec.ID + `","secret_name_for_workflow":"tracker_token"}`
	code, raw = teamCall(t, ts, "POST", "/api/teams/tenant-A/bots/review-pr/bindings", sre, body)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("binding across spellings refused (the ticket's misleading 400): %d %s", code, raw)
	}

	// The publisher at launch resolves bindings under the run's tenant —
	// the UUID.
	list, err := srv.botBindings.ListByTenantBot(store.WithTenant(context.Background(), "tenant-A"), "tenant-A", "review-pr")
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(list) != 1 || list[0].TenantID != "tenant-A" {
		t.Fatalf("the team's runs resolve %+v bindings, want the one written, keyed by the UUID", len(list))
	}
}

// The rewrite itself: a team route's slug becomes the UUID, its UUID is
// left alone (the id is the authority), a non-team route's {id} — even one
// spelling a team slug verbatim — is nobody's team, and an unknown
// spelling passes through.
func TestCanonicalizeTeamPathValue(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	cases := []struct {
		name, pattern, raw, want string
	}{
		{"team route by slug", "GET /api/teams/{id}/secrets", "team-a", "tenant-A"},
		{"team route by UUID", "GET /api/teams/{id}/secrets", "tenant-A", "tenant-A"},
		{"the team itself", "GET /api/teams/{id}", "team-a", "tenant-A"},
		{"a run id that spells the slug", "GET /api/runs/{id}", "team-a", "team-a"},
		{"an org id that spells the slug", "GET /api/orgs/{id}", "team-a", "team-a"},
		{"an unknown spelling", "GET /api/teams/{id}/secrets", "ghost", "ghost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/x", nil)
			r.SetPathValue("id", tc.raw)
			r.Pattern = tc.pattern
			srv.canonicalizeTeamPathValue(r)
			if got := r.PathValue("id"); got != tc.want {
				t.Errorf("PathValue = %q, want %q", got, tc.want)
			}
		})
	}
}

// The canonicalization changes no access decision: a caller with no
// standing on the team is 403 by either spelling, and an unresolvable
// spelling keeps its old semantics (a super-admin's list of a ghost team
// is the empty 200 it always was).
func TestTeamSlugCanonicalizationKeepsTheAccessSemantics(t *testing.T) {
	srv, ts := newTeamSlugServer(t)
	// u-member-b is a member of tenant-B alone: neither spelling of
	// tenant-A admits them.
	stranger := bearer(t, srv, caller("u-member-b", "tenant-B"))
	for _, spelling := range []string{"team-a", "tenant-A"} {
		if code, raw := teamCall(t, ts, "GET", "/api/teams/"+spelling+"/secrets", stranger, ""); code != http.StatusForbidden {
			t.Fatalf("%s: a caller without standing got %d, want 403 (%s)", spelling, code, raw)
		}
	}
	sre := bearer(t, srv, auth.Identity{UserID: "sre", IsSuperAdmin: true})
	if code, raw := teamCall(t, ts, "GET", "/api/teams/ghost/secrets", sre, ""); code != http.StatusOK {
		t.Fatalf("an unknown spelling changed semantics: %d %s", code, raw)
	}
}

// The precedence witness: team D's slug IS team C's UUID. The id is the
// authority, so the colliding string resolves team C — a write through it
// keys C's rows, never D's — and D is reached only by its own UUID: a
// member of D is 403 through the string (they hold no standing on C) and
// 200 through D's UUID. Inverting the precedence (slug first) turns this
// red: the string would then resolve D, and C's rows would be written to D.
func TestTeamSlugCollisionResolvesTheUUIDFirst(t *testing.T) {
	srv, ts := newTeamSlugServer(t)
	ctx := context.Background()
	// Team C is the fixture's tenant-A (slug team-a). Team D's slug is C's
	// UUID verbatim.
	if _, err := srv.authStore().CreateTeam(ctx, identity.Team{ID: "tenant-D", Slug: "tenant-A", Name: "Team D", OrgID: "org-1"}); err != nil {
		t.Fatalf("seed team D: %v", err)
	}
	if err := srv.authStore().UpsertMembership(ctx, identity.Membership{UserID: "u-member-d", TeamID: "tenant-D", Role: identity.RoleAdmin}); err != nil {
		t.Fatalf("seed D membership: %v", err)
	}
	sre := bearer(t, srv, auth.Identity{UserID: "sre", IsSuperAdmin: true})

	// A write through the colliding string lands on team C.
	code, raw := teamCall(t, ts, "POST", "/api/teams/tenant-A/secrets", sre, `{"name":"collision_probe","secret":"s3cr3t-value"}`)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create through the colliding string: %d %s", code, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.ID == "" {
		t.Fatalf("bad create body %s (%v)", raw, err)
	}
	if _, err := srv.genericSecrets.Get(store.WithTenant(ctx, "tenant-A"), created.ID); err != nil {
		t.Fatalf("the write through the colliding string did not key team C: %v", err)
	}
	if _, err := srv.genericSecrets.Get(store.WithTenant(ctx, "tenant-D"), created.ID); err == nil {
		t.Fatal("the write through the colliding string keyed team D — the slug outranked the UUID")
	}

	// A member of D alone: 403 through the string (it resolves C), 200
	// through D's own UUID.
	memberD := bearer(t, srv, caller("u-member-d", "tenant-D"))
	if code, raw := teamCall(t, ts, "GET", "/api/teams/tenant-A/secrets", memberD, ""); code != http.StatusForbidden {
		t.Fatalf("a member of D through the colliding string: %d, want 403 (%s)", code, raw)
	}
	if code, raw := teamCall(t, ts, "GET", "/api/teams/tenant-D/secrets", memberD, ""); code != http.StatusOK {
		t.Fatalf("a member of D through D's UUID: %d, want 200 (%s)", code, raw)
	}
}

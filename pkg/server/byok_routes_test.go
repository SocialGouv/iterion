package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// tenantSpyKeyStore records the tenant the handler put on the context of each
// write — the value the real (Mongo) store stamps onto the row and later
// filters reads by.
type tenantSpyKeyStore struct {
	secrets.ApiKeyStore
	createTenant string
}

func (s *tenantSpyKeyStore) Create(ctx context.Context, k secrets.ApiKey) error {
	s.createTenant, _ = store.TenantFromContext(ctx)
	return s.ApiKeyStore.Create(ctx, k)
}

// The composition the unit tests below cannot show: a team admin creating a key
// FOR another team must have it stamped with THAT team, or the runs it funds
// resolve nothing and fall back to the platform credential without a word.
func TestCreateTeamApiKey_StampsThePathTeam(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	spy := &tenantSpyKeyStore{ApiKeyStore: secrets.NewMemoryApiKeyStore()}
	srv := &Server{apiKeys: spy, sealer: sealer}

	body := `{"provider":"anthropic","name":"for-team-b","secret":"sk-ant-api-x"}`
	r := httptest.NewRequest("POST", "/api/teams/team-b/api-keys", strings.NewReader(body))
	r.SetPathValue("id", "team-b")
	// What requireAuth stamps: the caller's ACTIVE team, which is NOT the one
	// the key is for. A super-admin identity clears canManageTeam without a
	// membership store.
	ctx := auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"})
	r = r.WithContext(store.WithTenant(ctx, "team-a"))

	w := httptest.NewRecorder()
	srv.handleCreateTeamApiKey(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("create: status %d body %s", w.Code, w.Body.String())
	}
	if spy.createTenant != "team-b" {
		t.Fatalf("the row must be stamped with the team it is scoped to; got tenant %q", spy.createTenant)
	}
}

// The api-keys store derives tenant_id from the context: on write it stamps
// the row, on read it filters. requireAuth stamps the CALLER'S ACTIVE team, so
// a route that names a different team in its path has to re-scope — otherwise
// a key created for that team is stamped with the caller's, and the runs it
// was meant to fund resolve nothing at all.
func TestApiKeyTenantCtx_UsesThePathTeamNotTheActiveOne(t *testing.T) {
	r, err := http.NewRequest("POST", "/api/teams/team-b/api-keys", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.SetPathValue("id", "team-b")
	r = r.WithContext(store.WithTenant(r.Context(), "team-a")) // what requireAuth stamped

	got, ok := store.TenantFromContext(apiKeyTenantCtx(r))
	if !ok || got != "team-b" {
		t.Fatalf("want the path team team-b, got %q (ok=%v)", got, ok)
	}
}

// The /api/me family names no team: there the caller's active tenant IS the
// scope, and re-scoping would have nothing to scope to.
func TestApiKeyTenantCtx_KeepsTheActiveTeamWhenNoPathTeam(t *testing.T) {
	r, err := http.NewRequest("POST", "/api/me/api-keys", nil)
	if err != nil {
		t.Fatal(err)
	}
	r = r.WithContext(store.WithTenant(r.Context(), "team-a"))

	got, ok := store.TenantFromContext(apiKeyTenantCtx(r))
	if !ok || got != "team-a" {
		t.Fatalf("want the active team team-a, got %q (ok=%v)", got, ok)
	}
}

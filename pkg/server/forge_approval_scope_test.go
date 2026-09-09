package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Org.ProvisionApprovalScope — the "BYOK is free, shared credentials are
// reviewed" reading of the approval gate. Three branches decide, and only
// one of the three failures is loud: waving a request past the gate because
// a store blinked spends the org's money with nothing downstream to catch it.

// failingApiKeys is an ApiKeyStore whose ListByTeam is down — the degraded
// read the gate must NOT interpret as "this team pays its own way".
type failingApiKeys struct {
	secrets.ApiKeyStore
}

func (failingApiKeys) ListByTeam(context.Context, string, string) ([]secrets.ApiKey, error) {
	return nil, errors.New("mongo: connection reset")
}

func scopedApprovalServer(t *testing.T, scope identity.ProvisionApprovalScope) (*Server, func()) {
	t.Helper()
	s, _, done := newApprovalTestServer(t)
	s.apiKeys = secrets.NewMemoryApiKeyStore()
	o, err := s.authStore().GetOrg(context.Background(), "o1")
	if err != nil {
		t.Fatal(err)
	}
	o.ProvisionApprovalScope = scope
	if err := s.authStore().UpdateOrg(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	return s, done
}

func seedTeamKey(t *testing.T, s *Server, teamID string) {
	t.Helper()
	id := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(s.sealer, id, []byte("sk-ant-api03-team-own-key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.apiKeys.Create(withTenantCtx(teamID), secrets.ApiKey{
		ID: id, ScopeTeamID: teamID, Provider: secrets.ProviderAnthropic,
		Name: "team byok", SealedSecret: sealed, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

// Under `all` (the default and the historical behaviour) a team admin's
// request is parked whether or not the team has a key.
func TestApprovalScope_allParksEvenAFundedTeam(t *testing.T) {
	s, done := scopedApprovalServer(t, identity.ProvisionApprovalAll)
	defer done()
	seedTeamKey(t, s, "t1")

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(firstConnID(t, s)), "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s, want 202 (parked)", w.Code, w.Body.String())
	}
}

// Under `shared_credentials` a team with its OWN key provisions directly:
// it answers to nobody for what it spends.
func TestApprovalScope_sharedCredentialsLetsAFundedTeamThrough(t *testing.T) {
	s, done := scopedApprovalServer(t, identity.ProvisionApprovalSharedCredentials)
	defer done()
	seedTeamKey(t, s, "t1")

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(firstConnID(t, s)), "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200 (provisioned directly)", w.Code, w.Body.String())
	}
}

// The same org still parks a team that brings nothing: its runs would be
// funded by the org tier, the pool or the platform — someone else's budget.
func TestApprovalScope_sharedCredentialsParksAnUnfundedTeam(t *testing.T) {
	s, done := scopedApprovalServer(t, identity.ProvisionApprovalSharedCredentials)
	defer done()

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(firstConnID(t, s)), "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s, want 202 (parked)", w.Code, w.Body.String())
	}
}

// A USER-scoped key does not fund the team. The owner of a webhook, board
// or schedule launch is a synthetic identity with no personal credential,
// so one member's key funds none of the automated runs this provisioning
// would create.
func TestApprovalScope_aPersonalKeyDoesNotFundTheTeam(t *testing.T) {
	s, done := scopedApprovalServer(t, identity.ProvisionApprovalSharedCredentials)
	defer done()
	id := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(s.sealer, id, []byte("sk-ant-api03-personal-key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.apiKeys.Create(withTenantCtx("t1"), secrets.ApiKey{
		ID: id, ScopeTeamID: "t1", ScopeUserID: "teamadmin", Provider: secrets.ProviderAnthropic,
		Name: "personal", SealedSecret: sealed, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(firstConnID(t, s)), "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s, want 202 — a personal key passed the gate for the whole team", w.Code, w.Body.String())
	}
}

// "I could not tell" must never read as "they pay their own way": that is
// the direction in which a mistake spends the org's money. A degraded
// credential read fails the request loudly instead.
func TestApprovalScope_credentialReadErrorFailsClosed(t *testing.T) {
	s, done := scopedApprovalServer(t, identity.ProvisionApprovalSharedCredentials)
	defer done()
	connID := firstConnID(t, s)
	s.apiKeys = failingApiKeys{ApiKeyStore: s.apiKeys}

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s, want 503 — the gate failed OPEN on a degraded read", w.Code, w.Body.String())
	}
}

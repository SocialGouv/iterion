package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// The class #2655 names at the read sites outside #1668's claims: a read that
// FAILED answers its failure — logged, generic body — and never a fact. Three
// surfaces, each with the MIRROR scenario where the fact is legitimate because
// the store ANSWERED: an isolated pair per site, since a guard that only ever
// sees the failing direction passes just as well on a route that refuses
// everything.

// The failure a reachable store answers is errStoreDown (declared by
// forge_app_degraded_read_test.go, the same class on the forge App read): not
// ErrNotFound, so nothing about it says the row is absent.

// failingProviderStore answers errStoreDown from Get and delegates the rest:
// the per-org SSO resolver's only read of a provider row.
type failingProviderStore struct {
	orgsso.Store
	err error
}

func (f failingProviderStore) Get(context.Context, string) (orgsso.OrgSSOProvider, error) {
	return orgsso.OrgSSOProvider{}, f.err
}

// The account reads use the house fail-the-Nth-call harness (storeGate,
// auth_views_test.go): it already gates GetUsersByIDs and GetUser BY NAME, so
// the roster's own read fails while the authorization read it runs first
// (GetOrgMembership) still answers — the difference a wrapper failing every
// read would hide behind a 403.

// seedEnabledOrgProvider writes an enabled per-org OIDC row and answers its
// slug, so a resolution that fails afterwards fails on the READ and not on a
// provider that was never configured.
func seedEnabledOrgProvider(t *testing.T, s *Server) string {
	t.Helper()
	sealed, err := orgsso.SealClientSecret(s.sealer, "p1", "s3cr3t")
	if err != nil {
		t.Fatal(err)
	}
	row := orgsso.OrgSSOProvider{
		ID: "p1", TenantID: "t1", Kind: orgsso.KindOIDC, Enabled: true,
		IssuerURL: "https://sso.acme.example/realms/main", ClientID: "iterion",
		SealedSecret: sealed, CreatedAt: time.Now(),
	}
	if err := s.orgSSO.Create(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	return row.OIDCSlug()
}

// TestSSOStart_anUnreadableProviderStoreIsNotAnUnknownProvider: /start used to
// answer 404 "unknown provider" for every resolver failure, so a tenant whose
// provider store was down read as a tenant whose SSO had been deleted — and
// nothing was logged.
func TestSSOStart_anUnreadableProviderStoreIsNotAnUnknownProvider(t *testing.T) {
	s := newOrgSSOTestServer(t)
	slug := seedEnabledOrgProvider(t, s)
	s.orgSSO = failingProviderStore{Store: s.orgSSO, err: errStoreDown}

	w := newServerErrorRecorder(s)
	r := httptest.NewRequest("GET", "/api/auth/oidc/"+slug+"/start", nil)
	r.SetPathValue("provider", slug)
	s.handleOIDCStart(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d body = %s, want 500: a failed read is not the fact that the provider is unknown", w.Code, w.Body.String())
	}
	logged := w.Diagnostic(t)
	if !strings.Contains(logged, "p1") || !strings.Contains(logged, errStoreDown.Error()) {
		t.Errorf("the cause did not survive in the log: %q, want the provider id and %q", logged, errStoreDown)
	}
}

// TestSSOStart_aProviderThatIsNotThereStillAnswers404 is the mirror: the
// store ANSWERED that the row is absent, which is a fact /start owes the
// caller as a 404 — the 500 above must not swallow it.
func TestSSOStart_aProviderThatIsNotThereStillAnswers404(t *testing.T) {
	s := newOrgSSOTestServer(t)

	w := newServerErrorRecorder(s)
	r := httptest.NewRequest("GET", "/api/auth/oidc/oidc-org-nope/start", nil)
	r.SetPathValue("provider", "oidc-org-nope")
	s.handleOIDCStart(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d body = %s, want 404 for a provider the store says is absent", w.Code, w.Body.String())
	}
}

// TestSSOCallback_anUnreadableProviderStoreBannersProviderUnavailable: the
// callback is a top-level navigation, so its answer is a banner code. It used
// to be "unknown_provider", whose SPA text tells the user to stop waiting and
// sign in with a password an SSO-only account does not have.
func TestSSOCallback_anUnreadableProviderStoreBannersProviderUnavailable(t *testing.T) {
	s := newOrgSSOTestServer(t)
	slug := seedEnabledOrgProvider(t, s)
	s.orgSSO = failingProviderStore{Store: s.orgSSO, err: errStoreDown}
	logs := captureFailureLogs(s)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/auth/oidc/"+slug+"/callback?state=x&code=y", nil)
	r.SetPathValue("provider", slug)
	s.handleOIDCCallback(w, r)

	if w.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", w.Code)
	}
	if want := "/login?sso_error=" + ssoErrProviderUnavailable; w.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want %q", w.Header().Get("Location"), want)
	}
	if got := logs.String(); !strings.Contains(got, errStoreDown.Error()) {
		t.Errorf("the cause did not survive in the log: %q", got)
	}
}

// TestSSOCallback_aProviderThatIsNotThereStillBannersUnknownProvider is the
// mirror of the banner: the resolver's ErrUnknownProvider still reaches the
// SPA as unknown_provider.
func TestSSOCallback_aProviderThatIsNotThereStillBannersUnknownProvider(t *testing.T) {
	s := newOrgSSOTestServer(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/auth/oidc/oidc-org-nope/callback?state=x&code=y", nil)
	r.SetPathValue("provider", "oidc-org-nope")
	s.handleOIDCCallback(w, r)

	if want := "/login?sso_error=" + ssoErrUnknownProvider; w.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want %q", w.Header().Get("Location"), want)
	}
}

// TestSSOResolver_anUnsealableSecretIsNotAnUnknownProvider: the row is there
// and enabled, so a client secret this deployment's sealer cannot open is ITS
// failure — the second arm the resolver used to collapse into "unknown".
func TestSSOResolver_anUnsealableSecretIsNotAnUnknownProvider(t *testing.T) {
	s := newOrgSSOTestServer(t)
	if err := s.orgSSO.Create(context.Background(), orgsso.OrgSSOProvider{
		ID: "p1", TenantID: "t1", Kind: orgsso.KindOIDC, Enabled: true,
		IssuerURL: "https://sso.acme.example/realms/main", ClientID: "iterion",
		// Sealed by nobody: the sealer refuses it.
		SealedSecret: []byte("not-a-sealed-secret"), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := s.resolveConnector(context.Background(), "oidc-org-p1")
	if !errors.Is(err, oidc.ErrProviderUnavailable) {
		t.Fatalf("resolveConnector err = %v, want ErrProviderUnavailable", err)
	}
	if errors.Is(err, oidc.ErrUnknownProvider) {
		t.Errorf("a secret the sealer cannot open answered as an unknown provider: %v", err)
	}
}

// TestOrgRoster_aFailedAccountReadIsNotANamelessMember: the twin of the team
// roster #1668 fixed. A failed account read rendered every member as an
// account with no email and no name, answered 200 — an org admin reading it
// as "these accounts lost their identity".
func TestOrgRoster_aFailedAccountReadIsNotANamelessMember(t *testing.T) {
	f := newAuthRouteFixture(t)
	// The fixture's account is an ADMIN of org o1 (seedOrgTreeFor), so the
	// roster's authorization read answers from the row and only the account
	// read below is armed to fail.
	f.ids.fail("GetUsersByIDs", 1)

	w := newServerErrorRecorder(f.s)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID})
	f.s.handleListOrgMembers(w, orgReq(ctx, "GET", "/api/orgs/o1/members", "", "o1"))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d body = %s, want 500: a failed account read is not a member with no name", w.Code, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, "\"user_id\"") {
		t.Errorf("the roster rendered a member from a failed read: %s", body)
	}
	logged := w.Diagnostic(t)
	if !strings.Contains(logged, "GetUsersByIDs") || !strings.Contains(logged, "o1") {
		t.Errorf("the cause did not survive in the log: %q, want the failed read and the org", logged)
	}
	// The store's own text names an internal host: it stays in the log.
	if strings.Contains(w.Body.String(), internalHost) {
		t.Errorf("the store's text reached the client: %s", w.Body.String())
	}
}

// TestOrgRoster_anAbsentAccountStillRendersItsRow is the mirror: a membership
// whose account the store ANSWERED is gone for still renders with its id and
// role — the fact the roster owes, and the one the 500 above must not eat.
func TestOrgRoster_anAbsentAccountStillRendersItsRow(t *testing.T) {
	f := newAuthRouteFixture(t)
	// A membership whose account row was never written: GetUsersByIDs
	// ANSWERS a map without it.
	if err := f.s.authStore().UpsertOrgMembership(context.Background(), identity.OrgMembership{
		UserID: "u-gone", OrgID: "o1", Role: identity.OrgRoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID})
	f.s.handleListOrgMembers(w, orgReq(ctx, "GET", "/api/orgs/o1/members", "", "o1"))

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s, want 200", w.Code, w.Body.String())
	}
	var got struct {
		Members []orgMemberView `json:"members"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var gone *orgMemberView
	for i, m := range got.Members {
		if m.UserID == "u-gone" {
			gone = &got.Members[i]
		}
	}
	if gone == nil || gone.Role != string(identity.OrgRoleMember) || gone.Email != "" {
		t.Fatalf("members = %+v, want the row of u-gone with its role and no email", got.Members)
	}
}

// TestMyGitHubOrgs_aFailedReadIsNotAnEmptyList: the list used to answer 200
// with no orgs when its read failed, so the "Create a GitHub App" form offered
// to redo a connect flow that had already succeeded.
func TestMyGitHubOrgs_aFailedReadIsNotAnEmptyList(t *testing.T) {
	f := newAuthRouteFixture(t)
	f.ids.fail("GetUser", 1)

	w := newServerErrorRecorder(f.s)
	r := httptest.NewRequest("GET", "/api/me/github-orgs", nil)
	r = r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID}))
	f.s.handleListMyGitHubOrgs(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d body = %s, want 500: a failed read is not an empty list of orgs", w.Code, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, "\"orgs\"") {
		t.Errorf("a failed read answered a list anyway: %s", body)
	}
	if logged := w.Diagnostic(t); !strings.Contains(logged, "GetUser") {
		t.Errorf("the cause did not survive in the log: %q", logged)
	}
}

// TestMyGitHubOrgs_anAnsweredReadStillAnswersItsList is the mirror: an account
// the store answers for — with orgs, or absent entirely — keeps its 200, since
// an empty list is the fact "nothing connected yet".
func TestMyGitHubOrgs_anAnsweredReadStillAnswersItsList(t *testing.T) {
	f := newAuthRouteFixture(t)
	s := f.s
	if _, err := s.authStore().PatchUser(context.Background(), f.user.User.ID, identity.UserPatch{
		GitHubOrgs: &[]string{"socialgouv"},
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		userID string
		want   string
	}{
		{"the orgs it connected", f.user.User.ID, `{"orgs":["socialgouv"]}`},
		{"an account the store says is absent", "u-gone", `{"orgs":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/api/me/github-orgs", nil)
			r = r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{UserID: tc.userID}))
			s.handleListMyGitHubOrgs(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d body = %s, want 200", w.Code, w.Body.String())
			}
			if got := strings.TrimSpace(w.Body.String()); got != tc.want {
				t.Fatalf("body = %s, want %s", got, tc.want)
			}
		})
	}
}

// unauthorizableGitHubConn is the deployment's GitHub connector whose
// authorize step fails — a discovery or a template error whose text carries
// the URL it dialled and the upstream body.
type unauthorizableGitHubConn struct{ err error }

func (unauthorizableGitHubConn) Name() string    { return "github" }
func (unauthorizableGitHubConn) Display() string { return "GitHub" }

func (c unauthorizableGitHubConn) AuthorizeURL(context.Context, string, string, string) (string, error) {
	return "", c.err
}

func (unauthorizableGitHubConn) ExchangeCode(context.Context, string, string, string) (oidc.ExternalUser, error) {
	return oidc.ExternalUser{}, errors.New("not reached")
}

func (unauthorizableGitHubConn) SupportsPKCE() bool { return false }

// TestGitHubOrgsStart_aFailedAuthorizeKeepsItsTextInTheLog: the start route
// passed the connector's own error text as its 5xx message. The shared writer
// already floors every 5xx body to the generic one (internal/httpx
// WriteErrorJSON), so what the client reads was ALREADY safe — measured, by
// the mutant that puts the raw text back and changes nothing a client sees.
// What the route owed, and did not answer, is the LOG: the failed operation
// has to be named there, or the cause arrives as a bare connector string
// with nothing saying which call produced it.
func TestGitHubOrgsStart_aFailedAuthorizeKeepsItsTextInTheLog(t *testing.T) {
	f := newAuthRouteFixture(t)
	reg := oidc.NewRegistry()
	reg.Register(unauthorizableGitHubConn{
		err: fmt.Errorf("github: discovery at https://%s/.well-known: %w", internalHost, errStoreDown),
	})
	f.s.oidcRegistry = reg
	f.s.forgeStates = newForgeStateStore(time.Minute)

	w := newServerErrorRecorder(f.s)
	r := httptest.NewRequest("GET", "/api/forge/github/orgs/start", nil)
	r = r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID}))
	f.s.handleStartGitHubOrgs(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d body = %s, want 500", w.Code, w.Body.String())
	}
	// The floor the shared writer already guarantees: no connector text in
	// the body. It is asserted, not credited to this change.
	if body := w.Body.String(); strings.Contains(body, internalHost) {
		t.Errorf("the connector's text reached the client: %s", body)
	}
	logged := w.Diagnostic(t)
	if !strings.Contains(logged, internalHost) {
		t.Errorf("the cause did not survive in the log: %q", logged)
	}
	// What this change adds: the log names the call that failed.
	if !strings.Contains(logged, "build the github orgs authorize URL") {
		t.Errorf("the log does not name the failed operation: %q", logged)
	}
}

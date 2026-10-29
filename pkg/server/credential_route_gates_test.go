package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/auth/wsticket"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	"github.com/SocialGouv/iterion/pkg/credusage"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/marketplace"
	"github.com/SocialGouv/iterion/pkg/pat"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The routes that mint a credential, grant or exercise platform authority, or
// change or remove a credential ask the chokepoint (auth.Service.CheckDerivation)
// before they act, over the real handler: the access token of a revoked
// session does none of it.

// newDerivationFixtureWith is the derivation fixture with configure applied
// to the server's Config on top of personal access tokens and WS tickets.
func newDerivationFixtureWith(t *testing.T, configure func(*Config)) *derivationFixture {
	t.Helper()
	pats := &hookedPATs{MemoryStore: pat.NewMemoryStore()}
	f := &derivationFixture{authRouteFixture: newAuthRouteFixtureWith(t, func(c *Config) {
		c.PATs = pats
		c.WSTickets = wsticket.NewMemoryStore(time.Minute)
		configure(c)
	}), pats: pats}
	f.srv = httptest.NewServer(f.s.Handler())
	t.Cleanup(f.srv.Close)
	return f
}

// newDerivationFixtureWithOrgSSO is the derivation fixture with the per-org
// SSO provider and domain routes registered.
func newDerivationFixtureWithOrgSSO(t *testing.T) *derivationFixture {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return newDerivationFixtureWith(t, func(c *Config) {
		c.OrgSSO = orgsso.NewMemoryStore()
		c.OrgDomains = orgsso.NewMemoryDomainStore()
		c.Sealer = sealer
	})
}

// withStubSSO wires the fixture's server with a stub IdP answering ext for
// every exchange.
func (f *derivationFixture) withStubSSO(ext oidc.ExternalUser) {
	reg := oidc.NewRegistry()
	reg.Register(stubSSO{ext: ext})
	f.s.oidcRegistry = reg
	f.s.oidcStates = oidc.NewMemoryStateStore(10 * time.Minute)
}

// superAdmin registers a platform super-admin and answers its live session.
func (f *derivationFixture) superAdmin(t *testing.T) auth.LoginResult {
	t.Helper()
	ctx := context.Background()
	reg, err := f.svc.Register(ctx, "admin@example.com", fixturePassword, "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("register the admin: %v", err)
	}
	yes := true
	if _, err := f.s.authStore().PatchUser(ctx, reg.User.ID, identity.UserPatch{IsSuperAdmin: &yes}); err != nil {
		t.Fatalf("make the admin a super-admin: %v", err)
	}
	login, err := f.svc.Login(ctx, "admin@example.com", fixturePassword, "ua", "ip")
	if err != nil {
		t.Fatalf("admin login: %v", err)
	}
	return login
}

// linkedSubjects answers the IdP subjects linked to userID.
func (f *derivationFixture) linkedSubjects(t *testing.T, userID string) map[string]bool {
	t.Helper()
	links, err := f.s.authStore().ListOIDCLinksByUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("list the links: %v", err)
	}
	out := map[string]bool{}
	for _, l := range links {
		out[l.ProviderUserID] = true
	}
	return out
}

// TestPlatformAdmin_ARevokedSessionsAccessTokenExercisesNoPlatformAuthority:
// a super-admin's access token whose session is signed out everywhere reads
// no platform-admin route, mints no temporary password for its own account
// and grants super-admin to no one; a demoted super-admin's still-valid
// token is refused too — the flag is read from the account, not the token.
func TestPlatformAdmin_ARevokedSessionsAccessTokenExercisesNoPlatformAuthority(t *testing.T) {
	f := newDerivationFixture(t)
	ctx := context.Background()
	admin := f.superAdmin(t)
	if code, _, body := f.call(t, http.MethodGet, "/api/admin/users", admin.AccessToken, ""); code != http.StatusOK {
		t.Fatalf("a live super-admin session lists the accounts: %d %s", code, body)
	}
	otherDevice, err := f.svc.Login(ctx, "admin@example.com", fixturePassword, "other-device", "ip2")
	if err != nil {
		t.Fatalf("admin login (other device): %v", err)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", otherDevice.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("sign out everywhere: %d %s", code, body)
	}
	before, err := f.s.authStore().GetUser(ctx, admin.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/admin/users", ""},
		{http.MethodPost, "/api/admin/users/" + admin.User.ID + "/reset-password", ""},
		{http.MethodPatch, "/api/admin/users/" + f.user.User.ID, `{"is_super_admin":true}`},
	} {
		if code, _, body := f.call(t, c.method, c.path, admin.AccessToken, c.body); code != http.StatusUnauthorized {
			t.Fatalf("%s %s with a revoked session's super-admin token: %d %s, want 401", c.method, c.path, code, body)
		}
	}
	after, err := f.s.authStore().GetUser(ctx, admin.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PasswordHash != before.PasswordHash || after.Status != before.Status {
		t.Fatal("a revoked session's super-admin token replaced its own account's password")
	}
	if u, err := f.s.authStore().GetUser(ctx, f.user.User.ID); err != nil || u.IsSuperAdmin {
		t.Fatalf("a revoked session's super-admin token granted super-admin (%v)", err)
	}

	live, err := f.svc.Login(ctx, "admin@example.com", fixturePassword, "ua", "ip")
	if err != nil {
		t.Fatalf("admin login: %v", err)
	}
	no := false
	if _, err := f.s.authStore().PatchUser(ctx, admin.User.ID, identity.UserPatch{IsSuperAdmin: &no}); err != nil {
		t.Fatal(err)
	}
	if code, _, body := f.call(t, http.MethodGet, "/api/admin/users", live.AccessToken, ""); code != http.StatusForbidden {
		t.Fatalf("a demoted super-admin's live token: %d %s, want 403", code, body)
	}
}

// platformAdminRoute reports whether a recorded route is a platform-admin
// route: every route under /api/admin/ and the plugin writes.
func platformAdminRoute(r RouteInfo) bool {
	if strings.HasPrefix(r.Pattern, "/api/admin/") || r.Pattern == "/api/admin" {
		return true
	}
	return strings.HasPrefix(r.Pattern, "/api/v1/plugins") && r.Method != http.MethodGet && r.Method != ""
}

// TestPlatformAdmin_EveryPlatformAdminRouteRefusesARevokedSessionsToken: every
// platform-admin route the server registers — every store of the family
// configured — answers 401 to a super-admin's access token whose session is
// signed out everywhere, before its handler runs.
func TestPlatformAdmin_EveryPlatformAdminRouteRefusesARevokedSessionsToken(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f := newDerivationFixtureWith(t, func(c *Config) {
		c.Sealer = sealer
		c.ApiKeys = secrets.NewMemoryApiKeyStore()
		c.OAuthForfait = secrets.NewMemoryOAuthStore()
		c.UsageCapSettings = usagecap.NewMemorySettingsStore()
		c.UsageCaps = usagecap.NewMemStore()
		c.BotSources = botsource.NewMemoryStore()
		c.CredUsage = credusage.NewMemoryCounter()
		c.BotRolesSettings = platformcfg.NewMemoryStore[platformcfg.BotRoles]()
		c.SandboxSettings = platformcfg.NewMemoryStore[platformcfg.Sandbox]()
		c.BotVarsSettings = platformcfg.NewMemoryStore[platformcfg.BotVars]()
		c.BudgetFloorSettings = platformcfg.NewMemoryStore[budgetfloor.Policy]()
		c.PlatformCredentialsSettings = platformcfg.NewMemoryStore[platformcfg.PlatformCredentials]()
		c.Audit = audit.NewMemoryStore()
		c.Queue = newFakeDLQQueue()
		c.RunnerPoolsSettings = platformcfg.NewMemoryStore[platformcfg.RunnerPools]()
	})
	ctx := context.Background()
	admin := f.superAdmin(t)
	otherDevice, err := f.svc.Login(ctx, "admin@example.com", fixturePassword, "other-device", "ip2")
	if err != nil {
		t.Fatal(err)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", otherDevice.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("sign out everywhere: %d %s", code, body)
	}
	n := 0
	for _, r := range f.s.mux.Routes() {
		if !platformAdminRoute(r) {
			continue
		}
		method := r.Method
		if method == "" {
			method = http.MethodGet
		}
		path := concretePath(r.Pattern)
		code, _, body := f.call(t, method, path, admin.AccessToken, "{}")
		if code != http.StatusUnauthorized {
			t.Errorf("%s %s with a revoked session's super-admin token: %d %.120s, want 401", method, path, code, body)
		}
		t.Logf("%s %s -> %d", method, r.Pattern, code)
		n++
	}
	t.Logf("%d platform-admin routes asked", n)
	if n < 61 {
		t.Fatalf("only %d platform-admin routes registered: the fixture lost a family", n)
	}
}

// TestPlatformAdmin_TheMarketplacePluginRoutesAskTheChokepoint: installing or
// uninstalling a plugin through the marketplace clones and runs code on the
// server — platform authority — so a super-admin's access token whose session
// is signed out everywhere does neither.
func TestPlatformAdmin_TheMarketplacePluginRoutesAskTheChokepoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	repo := t.TempDir()
	writePluginFixtureDir(t, repo, "my-plugin")
	registry, err := marketplace.NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := newDerivationFixtureWith(t, func(c *Config) { c.Marketplace = registry })
	ctx := context.Background()
	admin := f.superAdmin(t)
	if code, _, body := f.call(t, http.MethodPost, "/api/v1/marketplace/submit", admin.AccessToken, `{"repo_url":"`+repo+`"}`); code != http.StatusOK {
		t.Fatalf("submit the plugin: %d %s", code, body)
	}
	otherDevice, err := f.svc.Login(ctx, "admin@example.com", fixturePassword, "other-device", "ip2")
	if err != nil {
		t.Fatal(err)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", otherDevice.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("sign out everywhere: %d %s", code, body)
	}
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		if code, _, body := f.call(t, method, "/api/v1/marketplace/bots/my-plugin/install", admin.AccessToken, ""); code != http.StatusUnauthorized {
			t.Fatalf("%s the marketplace plugin with a revoked session's super-admin token: %d %s, want 401", method, code, body)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "plugins", "my-plugin")); !os.IsNotExist(err) {
		t.Fatalf("a revoked session's super-admin token installed the plugin (%v)", err)
	}
	live, err := f.svc.Login(ctx, "admin@example.com", fixturePassword, "ua", "ip")
	if err != nil {
		t.Fatal(err)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/v1/marketplace/bots/my-plugin/install", live.AccessToken, ""); code != http.StatusOK {
		t.Fatalf("a live super-admin session installing the plugin: %d %s", code, body)
	}
}

// TestPlatformAdmin_APersonalAccessTokenResetsAnotherAccountsPasswordNotItsOwn:
// a super-admin's personal access token keeps the platform-admin routes — its
// row is read at every use — but a temporary password for its own account
// is a credential it may not mint.
func TestPlatformAdmin_APersonalAccessTokenResetsAnotherAccountsPasswordNotItsOwn(t *testing.T) {
	f := newDerivationFixture(t)
	ctx := context.Background()
	admin := f.superAdmin(t)
	code, pat := f.mintPAT(t, admin.AccessToken, "admin-ci")
	if code != http.StatusCreated || pat == "" {
		t.Fatalf("mint the admin's PAT: %d", code)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/admin/users/"+f.user.User.ID+"/reset-password", pat, ""); code != http.StatusOK || !strings.Contains(body, "temp_password") {
		t.Fatalf("a super-admin PAT resetting another account: %d %s, want 200 with a temporary password", code, body)
	}
	before, err := f.s.authStore().GetUser(ctx, admin.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/admin/users/"+admin.User.ID+"/reset-password", pat, ""); code != http.StatusForbidden || strings.Contains(body, "temp_password") {
		t.Fatalf("a super-admin PAT resetting its own account: %d %s, want 403", code, body)
	}
	if after, err := f.s.authStore().GetUser(ctx, admin.User.ID); err != nil || after.PasswordHash != before.PasswordHash {
		t.Fatalf("a PAT minted a password for its own account (%v)", err)
	}
}

// TestSSOLinks_SignOutEverywhereAndADisableRemoveWhatASessionLinked: a link
// made from the account settings records the session that made it; "sign out
// everywhere" and an admin disable remove every such link, and the link an
// SSO sign-in wrote itself — what an SSO-only account signs in with — stays.
func TestSSOLinks_SignOutEverywhereAndADisableRemoveWhatASessionLinked(t *testing.T) {
	f := newDerivationFixture(t)
	ctx := context.Background()
	if err := f.s.authStore().UpsertOIDCLink(ctx, identity.OIDCLink{Provider: "google", ProviderUserID: "signup-sub", UserID: f.user.User.ID, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "linked-sub", Email: "thief@evil.example"})
	if target := f.ssoRoundTrip(t, "/api/me/sso/testsso/link/start", f.user.AccessToken, nil); !strings.Contains(target, "sso_linked=") {
		t.Fatalf("the link from a live session landed on %q", target)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", f.other.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("sign out everywhere: %d %s", code, body)
	}
	if got := f.linkedSubjects(t, f.user.User.ID); got["linked-sub"] || !got["signup-sub"] {
		t.Fatalf("after sign out everywhere the account links %v, want the session's link removed and the sign-up link kept", got)
	}
	f.ssoRoundTrip(t, "/api/auth/oidc/testsso/start", "", nil)
	if link, err := f.s.authStore().GetOIDCLink(ctx, "testsso", "linked-sub"); err == nil && link.UserID == f.user.User.ID {
		t.Fatal("the removed identity still signs in as the account")
	}

	again, err := f.svc.Login(ctx, fixtureEmail, fixturePassword, "ua", "ip")
	if err != nil {
		t.Fatalf("login again: %v", err)
	}
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "linked-again", Email: fixtureEmail})
	if target := f.ssoRoundTrip(t, "/api/me/sso/testsso/link/start", again.AccessToken, nil); !strings.Contains(target, "sso_linked=") {
		t.Fatalf("the second link landed on %q", target)
	}
	admin := f.superAdmin(t)
	if code, _, body := f.call(t, http.MethodPatch, "/api/admin/users/"+f.user.User.ID, admin.AccessToken, `{"status":"disabled"}`); code != http.StatusOK {
		t.Fatalf("admin disable: %d %s", code, body)
	}
	if got := f.linkedSubjects(t, f.user.User.ID); got["linked-again"] || !got["signup-sub"] {
		t.Fatalf("after the admin disable the account links %v, want the session's link removed and the sign-up link kept", got)
	}
}

// linkWriteHook wraps the identity store: beforeWrite runs once inside the
// link's write, after the callback's first check; failGetUser fails the next
// account read; writeErr makes the write land and then report failure (the
// lost-commit class: the row exists, the answer says it does not).
type linkWriteHook struct {
	identity.Store
	beforeWrite func()
	failGetUser atomic.Bool
	writeErr    atomic.Bool
}

func (h *linkWriteHook) UpsertOIDCLink(ctx context.Context, link identity.OIDCLink) error {
	if fire := h.beforeWrite; fire != nil {
		h.beforeWrite = nil
		fire()
	}
	err := h.Store.UpsertOIDCLink(ctx, link)
	if h.writeErr.Load() {
		return errors.New("identity: the write landed but its answer was lost")
	}
	return err
}

func (h *linkWriteHook) GetUser(ctx context.Context, id string) (identity.User, error) {
	if h.failGetUser.CompareAndSwap(true, false) {
		return identity.User{}, errors.New("identity store unreachable")
	}
	return h.Store.GetUser(ctx, id)
}

// TestSSOLinks_TheCallbackAsksAgainOnceTheLinkIsWritten: a "sign out
// everywhere" landing between the callback's check and its write removes the
// account's session links before this one exists; the callback asks again
// once it does and removes it — and does the same when that second question
// cannot be answered.
func TestSSOLinks_TheCallbackAsksAgainOnceTheLinkIsWritten(t *testing.T) {
	for _, tc := range []struct {
		name        string
		beforeWrite func(t *testing.T, f *derivationFixture, h *linkWriteHook) func()
	}{
		{"a sign out everywhere lands inside the write", func(t *testing.T, f *derivationFixture, _ *linkWriteHook) func() {
			return func() {
				if code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", f.other.AccessToken, ""); code != http.StatusNoContent {
					t.Errorf("sign out everywhere: %d %s", code, body)
				}
			}
		}},
		{"the second check cannot complete", func(_ *testing.T, _ *derivationFixture, h *linkWriteHook) func() {
			return func() { h.failGetUser.Store(true) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDerivationFixture(t)
			f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "racer-sub", Email: "racer@evil.example"})
			h := &linkWriteHook{Store: f.s.authStore()}
			h.beforeWrite = tc.beforeWrite(t, f, h)
			f.svc.SetStoreForTest(h)
			target := f.ssoRoundTrip(t, "/api/me/sso/testsso/link/start", f.user.AccessToken, nil)
			if !strings.Contains(target, "sso_link_error=") {
				t.Fatalf("the callback landed on %q, want an sso_link_error", target)
			}
			if _, err := h.GetOIDCLink(context.Background(), "testsso", "racer-sub"); !errors.Is(err, identity.ErrNotFound) {
				t.Fatalf("the link the callback wrote is still there (%v)", err)
			}
		})
	}
}

// TestSSOLinks_AnAccessTokenNamingNoSessionLinksNothing: an access token
// minted before access tokens named their session starts no link — a link
// outlives every expiry, and nothing ties it to a session to revoke.
func TestSSOLinks_AnAccessTokenNamingNoSessionLinksNothing(t *testing.T) {
	f := newDerivationFixture(t)
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "legacy-sub", Email: "legacy@evil.example"})
	id, err := f.s.signer.Verify(f.user.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	id.SessionLineage, id.SessionGeneration = "", 0
	legacy, _, err := f.s.signer.IssueAccess(id)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, body := f.call(t, http.MethodGet, "/api/me/sso/testsso/link/start", legacy, ""); code != http.StatusForbidden {
		t.Fatalf("a link started by an access token naming no session: %d %s, want 403", code, body)
	}
	if _, err := f.s.authStore().GetOIDCLink(context.Background(), "testsso", "legacy-sub"); err == nil {
		t.Fatal("an access token naming no session linked an identity")
	}
}

// TestCredentialRemovals_ARevokedSessionsAccessTokenChangesNoCredential: once
// its session is signed out, an access token still within its lifetime
// removes no SSO identity, revokes no personal access token, changes no
// password, runs no sign out everywhere and changes no org sign-in — and a
// live session's does.
func TestCredentialRemovals_ARevokedSessionsAccessTokenChangesNoCredential(t *testing.T) {
	f := newDerivationFixtureWithOrgSSO(t)
	ctx := context.Background()
	if err := f.s.authStore().UpsertOIDCLink(ctx, identity.OIDCLink{Provider: "testsso", ProviderUserID: "own-sub", UserID: f.user.User.ID, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	code, pat := f.mintPAT(t, f.user.AccessToken, "ci")
	if code != http.StatusCreated {
		t.Fatalf("mint a PAT: %d", code)
	}
	toks, err := f.pats.ListByUser(ctx, f.user.User.ID)
	if err != nil || len(toks) != 1 {
		t.Fatalf("list the PATs: %v (%d)", err, len(toks))
	}
	if err := f.svc.Logout(ctx, f.user.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodDelete, "/api/me/sso/links/testsso/own-sub", ""},
		{http.MethodDelete, "/api/me/tokens/" + toks[0].ID, ""},
		{http.MethodPost, "/api/me/password", `{"current_password":"` + fixturePassword + `","new_password":"another-password-7"}`},
		{http.MethodPost, "/api/me/sessions/revoke-all", ""},
		{http.MethodPost, "/api/orgs/org-1/sso/providers", `{"kind":"oidc","display_name":"x"}`},
		{http.MethodPatch, "/api/orgs/org-1/sso/providers/p-1", `{"kind":"oidc","display_name":"x"}`},
		{http.MethodDelete, "/api/orgs/org-1/sso/providers/p-1", ""},
		{http.MethodPost, "/api/orgs/org-1/sso/domains", `{"domain":"example.org"}`},
		{http.MethodPost, "/api/orgs/org-1/sso/domains/d-1/verify", ""},
		{http.MethodDelete, "/api/orgs/org-1/sso/domains/d-1", ""},
	} {
		if code, _, body := f.call(t, c.method, c.path, f.user.AccessToken, c.body); code != http.StatusUnauthorized {
			t.Fatalf("%s %s with a signed-out session's access token: %d %s, want 401", c.method, c.path, code, body)
		}
	}
	if !f.linkedSubjects(t, f.user.User.ID)["own-sub"] {
		t.Fatal("a signed-out session's access token removed the account's SSO identity")
	}
	if got := f.meStatus(t, pat); got != http.StatusOK {
		t.Fatalf("a signed-out session's access token revoked the PAT: /auth/me with it %d", got)
	}
	if _, err := f.svc.Login(ctx, fixtureEmail, fixturePassword, "ua", "ip"); err != nil {
		t.Fatalf("a signed-out session's access token changed the password: %v", err)
	}
	if _, err := f.svc.Refresh(ctx, f.other.RefreshToken, "ua", "ip"); err != nil {
		t.Fatalf("a signed-out session's access token signed the account out everywhere: %v", err)
	}
	if code, _, body := f.call(t, http.MethodDelete, "/api/me/sso/links/testsso/own-sub", f.other.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("a live session removing its account's SSO identity: %d %s", code, body)
	}
}

// TestWSTicket_APersonalAccessTokensTicketDiesWithTheToken: a WS ticket a
// personal access token minted opens no WebSocket once the token is revoked.
func TestWSTicket_APersonalAccessTokensTicketDiesWithTheToken(t *testing.T) {
	f := newDerivationFixture(t)
	ctx := context.Background()
	code, pat := f.mintPAT(t, f.user.AccessToken, "ci")
	if code != http.StatusCreated {
		t.Fatalf("mint a PAT: %d", code)
	}
	mint := func() string {
		t.Helper()
		code, _, body := f.call(t, http.MethodPost, "/api/ws/ticket", pat, "")
		if code != http.StatusOK {
			t.Fatalf("mint a WS ticket with the PAT: %d %s", code, body)
		}
		var v struct {
			Ticket string `json:"ticket"`
		}
		if err := json.Unmarshal([]byte(body), &v); err != nil || v.Ticket == "" {
			t.Fatalf("the ticket: %v %s", err, body)
		}
		return v.Ticket
	}
	dial := func(ticket string) int {
		t.Helper()
		conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.srv.URL, "http")+"/api/ws?ticket="+url.QueryEscape(ticket), nil)
		if err == nil {
			_ = conn.Close()
			return resp.StatusCode
		}
		if resp == nil {
			t.Fatalf("dial: %v", err)
		}
		return resp.StatusCode
	}
	if got := dial(mint()); got != http.StatusSwitchingProtocols {
		t.Fatalf("the ticket of a live PAT dials %d, want 101", got)
	}
	ticket := mint()
	toks, err := f.pats.ListByUser(ctx, f.user.User.ID)
	if err != nil || len(toks) != 1 {
		t.Fatalf("list the PATs: %v", err)
	}
	if code, _, body := f.call(t, http.MethodDelete, "/api/me/tokens/"+toks[0].ID, f.user.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("revoke the PAT: %d %s", code, body)
	}
	if got := dial(ticket); got != http.StatusUnauthorized {
		t.Fatalf("the ticket of a revoked PAT dials %d, want 401", got)
	}
}

// TestDesktopExchange_ATicketWhoseSessionWasRevokedSinceDeliversNothing: the
// standing of the login a desktop SSO ticket defers is asked before the
// session is minted: a password change or a disable landing between the IdP
// callback and the exchange leaves the ticket nothing to mint.
func TestDesktopExchange_ATicketWhoseSessionWasRevokedSinceDeliversNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke func(t *testing.T, f *derivationFixture)
		want   int
	}{
		{"nothing happened since", func(*testing.T, *derivationFixture) {}, http.StatusOK},
		{"a password change", func(t *testing.T, f *derivationFixture) {
			if _, err := f.svc.ChangePassword(context.Background(), f.user.User.ID, fixturePassword, "another-password-7", "owner", "ip"); err != nil {
				t.Fatal(err)
			}
		}, http.StatusUnauthorized},
		{"an admin disable", func(t *testing.T, f *derivationFixture) {
			disabled := identity.UserStatusDisabled
			if _, err := f.s.authStore().PatchUser(context.Background(), f.user.User.ID, identity.UserPatch{Status: &disabled}); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.RevokeUserSessions(context.Background(), f.user.User.ID); err != nil {
				t.Fatal(err)
			}
		}, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDerivationFixture(t)
			ticket := mintDesktopTicket(t, f.authRouteFixture, f.user.User.ID)
			tc.revoke(t, f)
			code, _, body := f.call(t, http.MethodPost, "/api/auth/desktop/exchange", "", `{"ticket":"`+ticket+`"}`)
			var sess AuthSessionResponse
			_ = json.Unmarshal([]byte(body), &sess)
			if code != tc.want || (tc.want != http.StatusOK && sess.AccessToken != "") {
				t.Fatalf("the exchange after %s: %d (access token delivered=%t), want %d", tc.name, code, sess.AccessToken != "", tc.want)
			}
		})
	}
}

// TestSSOLinks_TheCallbackRemovesALinkWhoseWriteAnswerWasLost: the link
// write landing but its answer being lost (an error after the row is
// written) is the lost-commit class — the row THIS flow wrote is removed
// before the caller is told anything; the same subject connected again by a
// LATER flow of a live session lands and stays.
func TestSSOLinks_TheCallbackRemovesALinkWhoseWriteAnswerWasLost(t *testing.T) {
	f := newDerivationFixture(t)
	ctx := context.Background()
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "lostwrite-sub", Email: "lost@evil.example"})
	h := &linkWriteHook{Store: f.s.authStore()}
	h.writeErr.Store(true)
	f.svc.SetStoreForTest(h)
	target := f.ssoRoundTrip(t, "/api/me/sso/testsso/link/start", f.user.AccessToken, nil)
	if !strings.Contains(target, "sso_link_error=") {
		t.Fatalf("the callback landed on %q, want an sso_link_error", target)
	}
	if _, err := h.GetOIDCLink(ctx, "testsso", "lostwrite-sub"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("the link whose write answer was lost is still there (%v)", err)
	}

	// The same identity connected again by a later flow of the still-live
	// session: the write lands, the answer is honest, the row stays.
	h.writeErr.Store(false)
	if target := f.ssoRoundTrip(t, "/api/me/sso/testsso/link/start", f.other.AccessToken, nil); !strings.Contains(target, "sso_linked=") {
		t.Fatalf("the retried connect landed on %q, want sso_linked", target)
	}
	link, err := h.GetOIDCLink(ctx, "testsso", "lostwrite-sub")
	if err != nil || link.SessionLineage == "" {
		t.Fatalf("the retried connect's link did not land (%+v, %v)", link, err)
	}
}

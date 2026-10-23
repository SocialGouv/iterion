package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/wsticket"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/pat"
)

// Every route that mints a credential from the one presented to it — the
// team and org switches, POST /api/me/tokens, POST /api/ws/ticket and the SSO
// link — goes through auth.Service.CheckDerivation, over the real handler.

// derivationFixture is the auth-route fixture with personal access tokens and
// WS tickets enabled, served by the full handler.
type derivationFixture struct {
	*authRouteFixture
	pats *hookedPATs
	srv  *httptest.Server
}

func newDerivationFixture(t *testing.T) *derivationFixture {
	t.Helper()
	pats := &hookedPATs{MemoryStore: pat.NewMemoryStore()}
	f := &derivationFixture{authRouteFixture: newAuthRouteFixtureWith(t, func(c *Config) {
		c.PATs = pats
		c.WSTickets = wsticket.NewMemoryStore(time.Minute)
	}), pats: pats}
	f.srv = httptest.NewServer(f.s.Handler())
	t.Cleanup(f.srv.Close)
	return f
}

// call sends method path with the bearer and body through the full handler.
func (f *derivationFixture) call(t *testing.T, method, path, bearer, body string) (int, http.Header, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

// mintPAT posts POST /api/me/tokens with the bearer and answers the status
// and the plaintext token it carries.
func (f *derivationFixture) mintPAT(t *testing.T, bearer, name string) (int, string) {
	t.Helper()
	code, _, body := f.call(t, http.MethodPost, "/api/me/tokens", bearer, `{"name":"`+name+`"}`)
	var v struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal([]byte(body), &v)
	return code, v.Token
}

// tokensOf counts the account's personal access tokens, and the usable ones.
func (f *derivationFixture) tokensOf(t *testing.T) (all, usable int) {
	t.Helper()
	toks, err := f.pats.ListByUser(context.Background(), f.user.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range toks {
		if tk.Usable(time.Now().UTC()) {
			usable++
		}
	}
	return len(toks), usable
}

// meStatus answers GET /api/auth/me with the bearer.
func (f *derivationFixture) meStatus(t *testing.T, bearer string) int {
	t.Helper()
	code, _, _ := f.call(t, http.MethodGet, "/api/auth/me", bearer, "")
	return code
}

// hookedPATs is the memory PAT store with a hook run once, right before or
// right after the next token write.
type hookedPATs struct {
	*pat.MemoryStore
	mu                        sync.Mutex
	beforeCreate, afterCreate func()
}

func (h *hookedPATs) Create(ctx context.Context, tk pat.Token) error {
	h.mu.Lock()
	before, after := h.beforeCreate, h.afterCreate
	h.beforeCreate, h.afterCreate = nil, nil
	h.mu.Unlock()
	if before != nil {
		before()
	}
	if err := h.MemoryStore.Create(ctx, tk); err != nil {
		return err
	}
	if after != nil {
		after()
	}
	return nil
}

// TestCredentialDerivation_APersonalAccessTokenMintsNoSessionCredential: a
// personal access token is pinned to its team and its own expiry: the
// switches, POST /api/me/tokens and the SSO link answer it 403 and mint
// nothing; it still mints the WS ticket its WebSocket needs, no longer-lived
// than itself.
func TestCredentialDerivation_APersonalAccessTokenMintsNoSessionCredential(t *testing.T) {
	f := newDerivationFixture(t)
	code, token := f.mintPAT(t, f.user.AccessToken, "ci")
	if code != http.StatusCreated || token == "" {
		t.Fatalf("the PAT minted from a live session: %d", code)
	}
	before, _ := f.tokensOf(t)
	for _, path := range []string{
		"/api/auth/me/team/" + f.user.ActiveTeamID,
		"/api/auth/me/org/" + f.user.ActiveOrgID,
		"/api/me/tokens",
	} {
		body := ""
		if path == "/api/me/tokens" {
			body = `{"name":"child"}`
		}
		code, hdr, resp := f.call(t, http.MethodPost, path, token, body)
		if code != http.StatusForbidden || !strings.Contains(resp, "personal access token") {
			t.Fatalf("POST %s with a personal access token: %d %s, want 403 naming the personal access token", path, code, resp)
		}
		if len(hdr.Values("Set-Cookie")) != 0 || strings.Contains(resp, "access_token") {
			t.Fatalf("POST %s with a personal access token minted a credential: %q %s", path, hdr.Values("Set-Cookie"), resp)
		}
	}
	if after, _ := f.tokensOf(t); after != before {
		t.Fatalf("a personal access token minted %d token(s)", after-before)
	}
	if code, _, resp := f.call(t, http.MethodGet, "/api/me/sso/testsso/link/start", token, ""); code != http.StatusForbidden {
		t.Fatalf("an SSO link started with a personal access token: %d %s, want 403", code, resp)
	}
	code, _, resp := f.call(t, http.MethodPost, "/api/ws/ticket", token, "")
	if code != http.StatusOK || !strings.Contains(resp, `"ticket"`) {
		t.Fatalf("a WS ticket minted with a personal access token: %d %s, want 200", code, resp)
	}
}

// revocationsOf are the ways a session of the fixture's account ends while
// its access token is still within its lifetime.
var revocationsOf = []struct {
	name   string
	revoke func(t *testing.T, f *derivationFixture)
}{
	{"its session signed out", func(t *testing.T, f *derivationFixture) {
		if err := f.svc.Logout(context.Background(), f.user.RefreshToken); err != nil {
			t.Fatalf("Logout: %v", err)
		}
	}},
	{"its account signed out everywhere from another device", func(t *testing.T, f *derivationFixture) {
		if code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", f.other.AccessToken, ""); code != http.StatusNoContent {
			t.Fatalf("sign out everywhere: %d %s", code, body)
		}
	}},
	{"its password changed on another device", func(t *testing.T, f *derivationFixture) {
		body := `{"current_password":"` + fixturePassword + `","new_password":"another-password-7"}`
		if code, _, resp := f.call(t, http.MethodPost, "/api/me/password", f.other.AccessToken, body); code != http.StatusOK {
			t.Fatalf("password change: %d %s", code, resp)
		}
	}},
}

// TestCredentialDerivation_ARevokedSessionsAccessTokenMintsNothing: the
// access token of a session that ended keeps its own minutes, and mints
// nothing in them — no switch, no personal access token, no WS ticket, no
// SSO link — whichever revocation ended it.
func TestCredentialDerivation_ARevokedSessionsAccessTokenMintsNothing(t *testing.T) {
	for _, rv := range revocationsOf {
		t.Run(rv.name, func(t *testing.T) {
			f := newDerivationFixture(t)
			rv.revoke(t, f)
			before, _ := f.tokensOf(t)
			for _, tc := range []struct{ method, path, body string }{
				{http.MethodPost, "/api/auth/me/team/" + f.user.ActiveTeamID, ""},
				{http.MethodPost, "/api/auth/me/org/" + f.user.ActiveOrgID, ""},
				{http.MethodPost, "/api/me/tokens", `{"name":"thief"}`},
				{http.MethodPost, "/api/ws/ticket", ""},
				{http.MethodGet, "/api/me/sso/testsso/link/start", ""},
			} {
				code, _, resp := f.call(t, tc.method, tc.path, f.user.AccessToken, tc.body)
				if code != http.StatusUnauthorized {
					t.Fatalf("%s %s with the access token of a session that ended: %d %s, want 401", tc.method, tc.path, code, resp)
				}
			}
			if after, _ := f.tokensOf(t); after != before {
				t.Fatalf("the access token of a session that ended minted %d personal access token(s)", after-before)
			}
		})
	}
}

// TestWSTicket_ATicketMintsNothingAndDiesWithTheCredentialThatMintedIt: a
// ticket presented to POST /api/ws/ticket mints nothing (403); a ticket whose
// minting session ended, or whose minting token expired, opens no WebSocket;
// expires_in never promises more than the minting token has left.
func TestWSTicket_ATicketMintsNothingAndDiesWithTheCredentialThatMintedIt(t *testing.T) {
	f := newDerivationFixture(t)
	mint := func(bearer string) (string, int) {
		t.Helper()
		code, _, body := f.call(t, http.MethodPost, "/api/ws/ticket", bearer, "")
		if code != http.StatusOK {
			t.Fatalf("mint a WS ticket: %d %s", code, body)
		}
		var v struct {
			Ticket    string `json:"ticket"`
			ExpiresIn int    `json:"expires_in"`
		}
		_ = json.Unmarshal([]byte(body), &v)
		return v.Ticket, v.ExpiresIn
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
	chained, _ := mint(f.user.AccessToken)
	if code, _, body := f.call(t, http.MethodPost, "/api/ws/ticket?ticket="+url.QueryEscape(chained), "", ""); code != http.StatusForbidden {
		t.Fatalf("a ticket presented to mint a ticket: %d %s, want 403", code, body)
	}

	id, err := f.s.signer.Verify(f.user.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	short, _, err := f.s.signer.IssueAccessUntil(id, time.Now().Add(1500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	shortTicket, expiresIn := mint(short)
	if expiresIn > 2 {
		t.Fatalf("a ticket minted from a token with 1.5 s left promises %d s", expiresIn)
	}
	time.Sleep(2 * time.Second)
	if code := dial(shortTicket); code != http.StatusUnauthorized {
		t.Fatalf("a ticket dialled past its minting token's expiry: %d, want 401", code)
	}

	live, _ := mint(f.user.AccessToken)
	if code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", f.other.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("sign out everywhere: %d %s", code, body)
	}
	if code := dial(live); code != http.StatusUnauthorized {
		t.Fatalf("a ticket dialled after its minting session was signed out everywhere: %d, want 401", code)
	}
}

// TestSignOutEverywhere_RevokesEveryPersonalAccessToken: "sign out
// everywhere" is the compromise response: a token a thief minted from a
// session before it dies with it, and so does every other. A password change
// keeps them — it is no compromise response, and a CI token survives it.
func TestSignOutEverywhere_RevokesEveryPersonalAccessToken(t *testing.T) {
	f := newDerivationFixture(t)
	code, thief := f.mintPAT(t, f.user.AccessToken, "thief")
	if code != http.StatusCreated {
		t.Fatalf("the thief's PAT: %d", code)
	}
	code, ci := f.mintPAT(t, f.other.AccessToken, "ci")
	if code != http.StatusCreated {
		t.Fatalf("the CI PAT: %d", code)
	}
	body := `{"current_password":"` + fixturePassword + `","new_password":"another-password-7"}`
	code, _, resp := f.call(t, http.MethodPost, "/api/me/password", f.other.AccessToken, body)
	if code != http.StatusOK {
		t.Fatalf("password change: %d %s", code, resp)
	}
	var changed AuthSessionResponse
	if err := json.Unmarshal([]byte(resp), &changed); err != nil || changed.AccessToken == "" {
		t.Fatalf("the password change answered no access token: %s", resp)
	}
	for name, tk := range map[string]string{"the thief's PAT": thief, "the CI PAT": ci} {
		if got := f.meStatus(t, tk); got != http.StatusOK {
			t.Fatalf("%s after a password change: %d, want 200 (a password change keeps personal access tokens)", name, got)
		}
	}
	if code, _, resp := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", changed.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("sign out everywhere: %d %s", code, resp)
	}
	for name, tk := range map[string]string{"the thief's PAT": thief, "the CI PAT": ci} {
		if got := f.meStatus(t, tk); got != http.StatusUnauthorized {
			t.Fatalf("%s after sign out everywhere: %d, want 401", name, got)
		}
	}
	if _, usable := f.tokensOf(t); usable != 0 {
		t.Fatalf("%d personal access token(s) usable after sign out everywhere", usable)
	}
}

// TestAdminDisable_RevokesEveryPersonalAccessToken: an admin disable revokes
// the account's personal access tokens, so re-enabling the account does not
// bring them back.
func TestAdminDisable_RevokesEveryPersonalAccessToken(t *testing.T) {
	f := newDerivationFixture(t)
	code, tk := f.mintPAT(t, f.user.AccessToken, "ci")
	if code != http.StatusCreated {
		t.Fatalf("the PAT: %d", code)
	}
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true, Credential: auth.CredentialAccessToken})
	for _, status := range []string{"disabled", "active"} {
		r := httptest.NewRequest(http.MethodPatch, "/api/admin/users/"+f.user.User.ID, strings.NewReader(`{"status":"`+status+`"}`))
		r.SetPathValue("id", f.user.User.ID)
		w := httptest.NewRecorder()
		f.s.handleAdminUpdateUser(w, r.WithContext(admin))
		if w.Code != http.StatusOK {
			t.Fatalf("set the account %s: %d %s", status, w.Code, w.Body.String())
		}
	}
	if got := f.meStatus(t, tk); got != http.StatusUnauthorized {
		t.Fatalf("the PAT after a disable and a re-enable: %d, want 401", got)
	}
}

// TestCreatePAT_ARevocationLandingWhileItMintsRevokesTheToken: a "sign out
// everywhere" whose record lands after the token route checked the session —
// before the token's row is written, or after — leaves no usable token: the
// route asks again once the row exists and revokes it, and the plaintext is
// never answered.
func TestCreatePAT_ARevocationLandingWhileItMintsRevokesTheToken(t *testing.T) {
	for _, when := range []string{"before the row is written", "after the row is written"} {
		t.Run(when, func(t *testing.T) {
			f := newDerivationFixture(t)
			signOutEverywhere := func() {
				if err := f.svc.RevokeUserSessions(context.Background(), f.user.User.ID); err != nil {
					t.Errorf("RevokeUserSessions: %v", err)
				}
				if _, err := f.s.revokeAccountPATs(context.Background(), f.user.User.ID); err != nil {
					t.Errorf("revoke the PATs: %v", err)
				}
			}
			if when == "before the row is written" {
				f.pats.beforeCreate = signOutEverywhere
			} else {
				f.pats.afterCreate = signOutEverywhere
			}
			code, token := f.mintPAT(t, f.user.AccessToken, "racing")
			if code != http.StatusUnauthorized || token != "" {
				t.Fatalf("a PAT minted while its session was signed out everywhere: %d (plaintext answered: %t), want 401 and none", code, token != "")
			}
			if all, usable := f.tokensOf(t); all != 1 || usable != 0 {
				t.Fatalf("after the race the account holds %d token(s), %d usable; want the one written, revoked", all, usable)
			}
		})
	}
}

// TestHandleLogout_ALineageNamesTheSessionItSignsOut: GET /api/auth/me names
// the lineage of the presented token; a sign-out naming another lineage than
// the presented token's signs nothing out, keeps the cookies and says so
// (X-Iterion-Logout: other-session); one naming its own signs it out.
func TestHandleLogout_ALineageNamesTheSessionItSignsOut(t *testing.T) {
	f := newDerivationFixture(t)
	code, _, body := f.call(t, http.MethodGet, "/api/auth/me", f.user.AccessToken, "")
	if code != http.StatusOK {
		t.Fatalf("/auth/me: %d %s", code, body)
	}
	var me AuthMeResponse
	if err := json.Unmarshal([]byte(body), &me); err != nil {
		t.Fatal(err)
	}
	id, err := f.s.signer.Verify(f.user.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if me.SessionLineage == "" || me.SessionLineage != id.SessionLineage {
		t.Fatalf("/auth/me named the lineage %q, want the access token's %q", me.SessionLineage, id.SessionLineage)
	}
	otherID, err := f.s.signer.Verify(f.other.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	logout := func(lineage string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/logout?lineage="+url.QueryEscape(lineage), nil)
		r.Header.Set("X-Iterion-Refresh", f.user.RefreshToken)
		w := httptest.NewRecorder()
		f.s.handleLogout(w, r)
		return w
	}
	w := logout(otherID.SessionLineage)
	if w.Code != http.StatusNoContent || w.Header().Get(LogoutOutcomeHeader) != LogoutOtherSession {
		t.Fatalf("a sign-out naming another lineage: %d %q, want 204 with %s: other-session", w.Code, w.Header().Get(LogoutOutcomeHeader), LogoutOutcomeHeader)
	}
	assertNoSetCookie(t, w, "a sign-out naming another lineage")
	if r := refreshWithHeader(f.s, f.user.RefreshToken); r.Code != http.StatusOK {
		t.Fatalf("the session a sign-out naming another lineage was presented with no longer refreshes: %d", r.Code)
	}
	refreshed := cookieValue(refreshWithHeader(f.s, f.other.RefreshToken), refreshCookieName)
	if refreshed == "" {
		t.Fatal("the other device's session no longer refreshes")
	}
	w = logout(me.SessionLineage)
	if w.Code != http.StatusNoContent || w.Header().Get(LogoutOutcomeHeader) != "" {
		t.Fatalf("a sign-out naming its own lineage: %d %q, want 204 without %s", w.Code, w.Header().Get(LogoutOutcomeHeader), LogoutOutcomeHeader)
	}
	if len(w.Result().Header.Values("Set-Cookie")) == 0 {
		t.Fatal("a sign-out of its own lineage cleared no cookie")
	}
	if r := refreshWithHeader(f.s, f.user.RefreshToken); r.Code != http.StatusUnauthorized {
		t.Fatalf("the signed-out session refreshed: %d", r.Code)
	}
}

// TestCredentialChangeRoutes_APasswordCommittedWithoutItsSessionAnswers409:
// a password change, a reset confirm and a forced rotation whose new password
// is committed but whose session could not be written answer 409
// password_changed_sign_in, clear the cookies, and the new password signs in.
func TestCredentialChangeRoutes_APasswordCommittedWithoutItsSessionAnswers409(t *testing.T) {
	for _, flow := range []string{"password change", "reset confirm", "forced rotation"} {
		t.Run(flow, func(t *testing.T) {
			f := newDerivationFixture(t)
			ctx := context.Background()
			var w *httptest.ResponseRecorder
			switch flow {
			case "password change":
				r := httptest.NewRequest(http.MethodPost, "/api/me/password", strings.NewReader(`{"current_password":"`+fixturePassword+`","new_password":"another-password-7"}`))
				r = r.WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: f.user.User.ID, Credential: auth.CredentialAccessToken}))
				f.sessions.failCreateArmed.Store(true)
				w = httptest.NewRecorder()
				f.s.handleChangeMyPassword(w, r)
			case "reset confirm":
				link := seedResetLink(t, f.authRouteFixture)
				r := httptest.NewRequest(http.MethodPost, "/api/auth/password/reset/confirm", strings.NewReader(`{"token":"`+link+`","password":"another-password-7"}`))
				f.sessions.failCreateArmed.Store(true)
				w = httptest.NewRecorder()
				f.s.handlePasswordResetConfirm(w, r)
			case "forced rotation":
				pending := identity.UserStatusPendingPasswordChange
				if _, err := f.s.authStore().PatchUser(ctx, f.user.User.ID, identity.UserPatch{Status: &pending}); err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest(http.MethodPost, "/api/auth/password/change", strings.NewReader(`{"email":"`+fixtureEmail+`","current_password":"`+fixturePassword+`","new_password":"another-password-7"}`))
				f.sessions.failCreateArmed.Store(true)
				w = httptest.NewRecorder()
				f.s.handleChangePassword(w, r)
			}
			if w.Code != http.StatusConflict {
				t.Fatalf("the %s whose session failed after the commit: %d %s, want 409", flow, w.Code, w.Body.String())
			}
			var v map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v["error_code"] != "password_changed_sign_in" || !strings.Contains(v["error"], "sign in with the new password") {
				t.Fatalf("the 409 body: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "cluster") {
				t.Fatalf("the 409 body carries the store's cause: %s", w.Body.String())
			}
			if len(w.Result().Header.Values("Set-Cookie")) == 0 {
				t.Fatal("the 409 cleared no cookie")
			}
			if _, err := f.svc.Login(ctx, fixtureEmail, "another-password-7", "ua", "ip"); err != nil {
				t.Fatalf("the new password after the 409: %v", err)
			}
		})
	}
}

// seedResetLink stores a reset link for the fixture's account.
func seedResetLink(t *testing.T, f *authRouteFixture) string {
	t.Helper()
	token := auth.ResetTokenPrefix + "route-reset-" + f.user.User.ID
	now := time.Now().UTC()
	if err := f.resets.Create(context.Background(), auth.PasswordReset{
		ID: "reset-" + f.user.User.ID, UserID: f.user.User.ID, TokenHash: auth.HashRefreshToken(token),
		CreatedAt: now, ExpiresAt: now.Add(auth.ResetTokenTTL),
	}); err != nil {
		t.Fatalf("seed a reset link: %v", err)
	}
	return token
}

// stubSSO is an OIDC connector whose IdP always answers ext.
type stubSSO struct{ ext oidc.ExternalUser }

func (stubSSO) Name() string    { return "testsso" }
func (stubSSO) Display() string { return "Test SSO" }
func (stubSSO) AuthorizeURL(_ context.Context, _, state, _ string) (string, error) {
	return "https://idp.example/authorize?state=" + url.QueryEscape(state), nil
}
func (c stubSSO) ExchangeCode(context.Context, string, string, string) (oidc.ExternalUser, error) {
	return c.ext, nil
}
func (stubSSO) SupportsPKCE() bool { return true }

// ssoRoundTrip starts an SSO flow at start (with the bearer, for a link) and
// completes its callback in the same browser, answering the callback's
// redirect target.
func (f *derivationFixture) ssoRoundTrip(t *testing.T, start, bearer string, between func()) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+start, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("%s: %d, want the redirect to the IdP", start, resp.StatusCode)
	}
	authURL, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if between != nil {
		between()
	}
	cb, err := http.NewRequest(http.MethodGet, f.srv.URL+"/api/auth/oidc/testsso/callback?code=c&state="+url.QueryEscape(authURL.Query().Get("state")), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Cookies() {
		cb.AddCookie(c)
	}
	out, err := client.Do(cb)
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Body.Close()
	return out.Header.Get("Location")
}

// TestOIDC_APendingAccountIsSentToItsExitAndALinkNeedsALiveSession: an SSO
// sign-in of an account pending a password change opens no session and lands
// on sso_error=password_change_required; a link started from a session that
// ends during the IdP round trip links nothing.
func TestOIDC_APendingAccountIsSentToItsExitAndALinkNeedsALiveSession(t *testing.T) {
	f := newDerivationFixture(t)
	reg := oidc.NewRegistry()
	reg.Register(stubSSO{ext: oidc.ExternalUser{Provider: "testsso", Subject: "sub-1", Email: fixtureEmail}})
	f.s.oidcRegistry = reg
	f.s.oidcStates = oidc.NewMemoryStateStore(10 * time.Minute)

	target := f.ssoRoundTrip(t, "/api/me/sso/testsso/link/start", f.user.AccessToken, func() {
		if err := f.svc.Logout(context.Background(), f.user.RefreshToken); err != nil {
			t.Fatalf("Logout: %v", err)
		}
	})
	if !strings.Contains(target, "sso_link_error=") {
		t.Fatalf("a link whose session ended during the round trip landed on %q, want an sso_link_error", target)
	}
	if _, err := f.s.authStore().GetOIDCLink(context.Background(), "testsso", "sub-1"); err == nil {
		t.Fatal("a link whose session ended during the round trip linked the identity")
	}

	target = f.ssoRoundTrip(t, "/api/me/sso/testsso/link/start", f.other.AccessToken, nil)
	if !strings.Contains(target, "sso_linked=") {
		t.Fatalf("a link from a live session landed on %q, want sso_linked", target)
	}
	pending := identity.UserStatusPendingPasswordChange
	if _, err := f.s.authStore().PatchUser(context.Background(), f.user.User.ID, identity.UserPatch{Status: &pending}); err != nil {
		t.Fatal(err)
	}
	target = f.ssoRoundTrip(t, "/api/auth/oidc/testsso/start", "", nil)
	if !strings.Contains(target, "sso_error=password_change_required") {
		t.Fatalf("an SSO sign-in of a pending account landed on %q, want sso_error=password_change_required", target)
	}
}

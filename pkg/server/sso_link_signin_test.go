package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// An account that signs in through SSO only never loses the last sign-in
// method it had — not to its own settings, not to "sign out everywhere", not
// to an admin disable — while a link another session wrote is still removed.

// TestAnSSOOnlyAccountKeepsItsSignInAfterSignOutEverywhere: an SSO-only
// account (no password) connects a second provider from the settings, drops
// its original sign-up identity (an IdP migration), then signs out
// everywhere. The removal pass keeps the account's remaining link — the one
// sign-in method it had — and the next SSO sign-in with it lands signed-in.
func TestAnSSOOnlyAccountKeepsItsSignInAfterSignOutEverywhere(t *testing.T) {
	f := newDerivationFixture(t)
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "orig-sub", Email: "sso-only@example.com"})
	cookies, _ := f.ssoStartAndCallback(t, "/api/auth/oidc/testsso/start", nil)
	access := f.accessViaCookies(t, cookies)
	if access == "" {
		t.Fatal("the SSO sign-up handed out no session")
	}
	uid := f.userIDViaCookies(t, cookies)
	if got := f.linkedSubjects(t, uid); !got["orig-sub"] {
		t.Fatalf("after the sign-up the account links %v", got)
	}

	// Connect a second provider from the settings (the session's lineage on it).
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "second-sub", Email: "sso-only@example.com"})
	if target := f.ssoRoundTrip(t, "/api/me/sso/testsso/link/start", access, nil); !contains(target, "sso_linked") {
		t.Fatalf("the second provider connect landed on %q", target)
	}

	// Drop the original sign-up identity: an explicit user act with another
	// method in place.
	if code, _, body := f.call(t, http.MethodDelete, "/api/me/sso/links/testsso/orig-sub", access, ""); code != http.StatusNoContent {
		t.Fatalf("unlink the original identity with a second provider in place: %d %s", code, body)
	}

	// Sign out everywhere: the session's links go, but the account keeps a
	// sign-in method it had.
	if code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", access, ""); code != http.StatusNoContent {
		t.Fatalf("sign out everywhere: %d %s", code, body)
	}
	got := f.linkedSubjects(t, uid)
	if !got["second-sub"] || len(got) != 1 {
		t.Fatalf("after sign out everywhere the SSO-only account links %v, want [second-sub]", got)
	}

	// The next SSO sign-in with the kept identity lands signed-in.
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "second-sub", Email: "sso-only@example.com"})
	again, land := f.ssoStartAndCallback(t, "/api/auth/oidc/testsso/start", nil)
	if contains(land, "sso_link_error") || contains(land, "link_required") {
		t.Fatalf("the SSO sign-in with the kept identity landed on %q", land)
	}
	if f.accessViaCookies(t, again) == "" {
		t.Fatalf("the SSO sign-in handed out no session (landed on %q)", land)
	}
}

// TestASessionAddedSSOLinkIsStillRemovedBySignOutEverywhere: a link another
// session wrote (an attacker's session, a rollout-overlap pod) is removed by
// "sign out everywhere" even though the account it hit signs in through SSO
// only — its own sign-up link (no lineage) stays, so nothing is kept against
// the account's interest.
func TestASessionAddedSSOLinkIsStillRemovedBySignOutEverywhere(t *testing.T) {
	f := newDerivationFixture(t)
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "orig-sub", Email: "sso-only@example.com"})
	cookies, _ := f.ssoStartAndCallback(t, "/api/auth/oidc/testsso/start", nil)
	uid := f.userIDViaCookies(t, cookies)

	// Another session (here: a second sign-in of another device) connects an
	// identity from the settings.
	other, land := f.ssoStartAndCallback(t, "/api/auth/oidc/testsso/start", nil)
	_ = land
	otherAccess := f.accessViaCookies(t, other)
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "attacker-sub", Email: "sso-only@example.com"})
	if target := f.ssoRoundTrip(t, "/api/me/sso/testsso/link/start", otherAccess, nil); !contains(target, "sso_linked") {
		t.Fatalf("the session-added link landed on %q", target)
	}

	if code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", otherAccess, ""); code != http.StatusNoContent {
		t.Fatalf("sign out everywhere: %d %s", code, body)
	}
	got := f.linkedSubjects(t, uid)
	if got["attacker-sub"] {
		t.Fatalf("the session-added link survived sign out everywhere: %v", got)
	}
	if !got["orig-sub"] {
		t.Fatalf("the sign-up link did not survive: %v", got)
	}
}

// TestAPasswordlessAccountKeepsItsOldestLinkWhenEveryLinkWasSessionWritten:
// the keep-one rule's shape — an account with no password whose every link
// carries a session lineage keeps the OLDEST of them; an account with a
// password keeps none.
func TestAPasswordlessAccountKeepsItsOldestLinkWhenEveryLinkWasSessionWritten(t *testing.T) {
	f := newDerivationFixture(t)
	ctx := context.Background()
	// An SSO-only account (no password) whose every link is session-written:
	// the state an old pod's rollout overlap or a reconnect could reach.
	if err := f.s.authStore().UpsertOIDCLink(ctx, identity.OIDCLink{
		Provider: "testsso", ProviderUserID: "old-sub", UserID: f.user.User.ID,
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour), SessionLineage: "lin-old",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.authStore().UpsertOIDCLink(ctx, identity.OIDCLink{
		Provider: "testsso", ProviderUserID: "new-sub", UserID: f.user.User.ID,
		CreatedAt: time.Now().UTC(), SessionLineage: "lin-new",
	}); err != nil {
		t.Fatal(err)
	}
	// The account has a password (the fixture user): both session links go.
	if code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", f.other.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("sign out everywhere: %d %s", code, body)
	}
	if got := f.linkedSubjects(t, f.user.User.ID); len(got) != 0 {
		t.Fatalf("an account with a password kept links: %v", got)
	}
}

// TestAnSSOOnlyAccountCannotRemoveItsLastSignInMethod: the settings path
// refuses (409 last_sign_in_method) to disconnect the only sign-in method of
// an account with no password.
func TestAnSSOOnlyAccountCannotRemoveItsLastSignInMethod(t *testing.T) {
	f := newDerivationFixture(t)
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "orig-sub", Email: "sso-only@example.com"})
	cookies, _ := f.ssoStartAndCallback(t, "/api/auth/oidc/testsso/start", nil)
	access := f.accessViaCookies(t, cookies)
	code, _, body := f.call(t, http.MethodDelete, "/api/me/sso/links/testsso/orig-sub", access, "")
	if code != http.StatusConflict || !contains(body, "last_sign_in_method") {
		t.Fatalf("removing the last sign-in method answered %d %s, want 409 last_sign_in_method", code, body)
	}
	if got := f.linkedSubjects(t, f.userIDViaCookies(t, cookies)); !got["orig-sub"] {
		t.Fatalf("the refused unlink removed the link anyway: %v", got)
	}
}

// ssoStartAndCallback walks the IdP round trip and keeps the cookies the
// callback hands out (an SSO sign-up's session).
func (f *derivationFixture) ssoStartAndCallback(t *testing.T, start string, between func()) ([]*http.Cookie, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+start, nil)
	if err != nil {
		t.Fatal(err)
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
	defer out.Body.Close()
	return out.Cookies(), out.Header.Get("Location")
}

// accessViaCookies answers the access JWT the sign-up handed out (the
// iterion_auth cookie).
func (f *derivationFixture) accessViaCookies(t *testing.T, cookies []*http.Cookie) string {
	t.Helper()
	for _, c := range cookies {
		if c.Name == "iterion_auth" || c.Name == "__Host-iterion_auth" {
			return c.Value
		}
	}
	return ""
}

// userIDViaCookies reads the account the browser cookies sign in as.
func (f *derivationFixture) userIDViaCookies(t *testing.T, cookies []*http.Cookie) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+"/api/auth/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/auth/me with the sign-up cookies: %d", resp.StatusCode)
	}
	var me struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	return me.User.ID
}

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/desktopsso"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/identity"
)

func TestSafeDesktopRedirect(t *testing.T) {
	ok := []string{
		"http://127.0.0.1:53123/callback",
		"http://127.0.0.1:1/",
		"http://localhost:8080/desktop/cb?x=1",
		"http://[::1]:40000/cb",
	}
	for _, v := range ok {
		if safeDesktopRedirect(v) != v {
			t.Errorf("safeDesktopRedirect(%q) rejected a valid loopback URL", v)
		}
	}
	bad := []string{
		"",
		"http://evil.com:80/cb",             // non-loopback host
		"https://127.0.0.1:53123/cb",        // not http
		"http://127.0.0.1/cb",               // no explicit port
		"http://127.0.0.1:53123/cb#frag",    // fragment
		"http://user:pw@127.0.0.1:53123/cb", // embedded credentials
		"ftp://127.0.0.1:53123/cb",          // wrong scheme
		"//127.0.0.1:53123/cb",              // scheme-relative
		"http://169.254.169.254:80/latest",  // link-local metadata, not loopback
		"http://127.0.0.1.evil.com:80/cb",   // host that only prefixes loopback
	}
	for _, v := range bad {
		if got := safeDesktopRedirect(v); got != "" {
			t.Errorf("safeDesktopRedirect(%q) = %q, want rejected", v, got)
		}
	}
}

func TestHandleDesktopExchange_Unavailable(t *testing.T) {
	// No authSvc / no ticket store → 404 (feature off), never a panic.
	s := &Server{}
	r := httptest.NewRequest(http.MethodPost, "/api/auth/desktop/exchange", strings.NewReader(`{"ticket":"x"}`))
	w := httptest.NewRecorder()
	s.handleDesktopExchange(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when exchange unavailable", w.Code)
	}
}

// recordingTickets wraps a desktopsso.Store and captures every payload minted
// through it — the recorder the #2635 witness reads: what a ticket stores is
// the deferred login, and no credential.
type recordingTickets struct {
	desktopsso.Store
	mu     sync.Mutex
	minted []auth.DesktopLogin
}

func (r *recordingTickets) Mint(ctx context.Context, d auth.DesktopLogin) (string, error) {
	r.mu.Lock()
	r.minted = append(r.minted, d)
	r.mu.Unlock()
	return r.Store.Mint(ctx, d)
}

func (r *recordingTickets) captured() []auth.DesktopLogin {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]auth.DesktopLogin{}, r.minted...)
}

// TestDesktopSSO_TheTicketCarriesNoCredentialAndNoSessionWaits drives the
// whole desktop flow — start, callback, exchange — over HTTP and asserts the
// #2635 contract: the callback mints a ticket whose stored payload is the
// DEFERRED login (the account, the lineage, the landing team) and nothing
// else — no token-shaped field, the ticket itself nowhere in it — and NO
// session exists while the ticket waits, so an unredeemed ticket leaves
// nothing to revoke. The redemption mints the session: 200, the cookies, and
// the access token in the body (the desktop is a native client).
func TestDesktopSSO_TheTicketCarriesNoCredentialAndNoSessionWaits(t *testing.T) {
	f := newDerivationFixture(t)
	rec := &recordingTickets{Store: desktopsso.NewMemoryStore(time.Minute)}
	f.s.desktopTickets = rec
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "desk-1", Email: "desk@example.org", Name: "Desk"})

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// The start, as the desktop's native client calls it.
	start, err := client.Get(f.srv.URL + "/api/auth/oidc/testsso/start?desktop=1&format=json&next=" + url.QueryEscape("http://127.0.0.1:41234/callback"))
	if err != nil {
		t.Fatal(err)
	}
	defer start.Body.Close()
	if start.StatusCode != http.StatusOK {
		t.Fatalf("the desktop start answered %d", start.StatusCode)
	}
	var started struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.NewDecoder(start.Body).Decode(&started); err != nil {
		t.Fatalf("decode the start: %v", err)
	}
	authURL, err := url.Parse(started.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	// The browser completes the callback — a desktop flow stores no agent
	// binding, so no cookie travels.
	cb, err := client.Get(f.srv.URL + "/api/auth/oidc/testsso/callback?code=c&state=" + url.QueryEscape(authURL.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Body.Close()
	if cb.StatusCode != http.StatusFound {
		t.Fatalf("the callback answered %d, want the 302 to the loopback", cb.StatusCode)
	}
	target, err := url.Parse(cb.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "127.0.0.1:41234" {
		t.Fatalf("the callback redirected to %q, want the validated loopback", target)
	}
	ticket := target.Query().Get("ticket")
	if ticket == "" {
		t.Fatalf("the callback's redirect carries no ticket: %q", target)
	}

	// The witness on the store: exactly one mint, whose payload marshals to
	// the deferred login and to no credential-shaped field, and which does
	// not hold the ticket itself.
	minted := rec.captured()
	if len(minted) != 1 {
		t.Fatalf("the callback minted %d ticket(s), want 1", len(minted))
	}
	d := minted[0]
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "token") || strings.Contains(string(raw), "secret") {
		t.Errorf("the minted ticket payload carries a credential-shaped field: %s", raw)
	}
	if strings.Contains(string(raw), ticket) {
		t.Errorf("the minted ticket payload holds the ticket itself: %s", raw)
	}

	// No session waits: the lineage the ticket names has no live row — an
	// unredeemed ticket leaves nothing to revoke at its expiry.
	if _, found, err := f.sessions.LiveLineageSession(context.Background(), d.UserID, d.Root.ID); err != nil || found {
		t.Fatalf("before the redemption the ticket's lineage is live (found=%v, err=%v)", found, err)
	}

	code, _, body := f.call(t, http.MethodPost, "/api/auth/desktop/exchange", "", `{"ticket":"`+ticket+`"}`)
	if code != http.StatusOK {
		t.Fatalf("the exchange answered %d: %s", code, body)
	}
	var sess AuthSessionResponse
	if err := json.Unmarshal([]byte(body), &sess); err != nil {
		t.Fatalf("decode the exchange: %v", err)
	}
	if sess.AccessToken == "" {
		t.Error("the exchange delivered no access token in the body — the desktop is a native client")
	}
	if sess.User.ID != d.UserID {
		t.Errorf("the exchange signed in %q, want the ticket's account %q", sess.User.ID, d.UserID)
	}
	// The session exists from the redemption on — under the lineage the
	// callback named.
	if _, found, err := f.sessions.LiveLineageSession(context.Background(), d.UserID, d.Root.ID); err != nil || !found {
		t.Fatalf("after the redemption the ticket's lineage is not live (found=%v, err=%v)", found, err)
	}
	// Single use: a second redemption is the invalid-ticket 401.
	if code, _, _ := f.call(t, http.MethodPost, "/api/auth/desktop/exchange", "", `{"ticket":"`+ticket+`"}`); code != http.StatusUnauthorized {
		t.Errorf("the second redemption answered %d, want 401", code)
	}
}

// TestDesktopSSO_ARefusedLoginRedirectsItsErrorToTheLoopback is the #2687
// witness: a desktop SSO sign-in the server REFUSES — the account is pending
// a password change — no longer lands its sso_error in the browser tab while
// the desktop waits out its five-minute timeout: the callback 302s the
// refusal to the desktop's own loopback listener, and no ticket is minted.
func TestDesktopSSO_ARefusedLoginRedirectsItsErrorToTheLoopback(t *testing.T) {
	f := newDerivationFixture(t)
	rec := &recordingTickets{Store: desktopsso.NewMemoryStore(time.Minute)}
	f.s.desktopTickets = rec
	ctx := context.Background()
	reg, err := f.svc.Register(ctx, "pending@example.org", fixturePassword, "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("register the pending account: %v", err)
	}
	pending := identity.UserStatusPendingPasswordChange
	if _, err := f.s.authStore().PatchUser(ctx, reg.User.ID, identity.UserPatch{Status: &pending}); err != nil {
		t.Fatalf("make the account pending a password change: %v", err)
	}
	if err := f.s.authStore().UpsertOIDCLink(ctx, identity.OIDCLink{
		Provider: "testsso", ProviderUserID: "desk-pending", UserID: reg.User.ID, Email: "pending@example.org",
	}); err != nil {
		t.Fatalf("link the SSO identity: %v", err)
	}
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "desk-pending", Email: "pending@example.org"})

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start, err := client.Get(f.srv.URL + "/api/auth/oidc/testsso/start?desktop=1&format=json&next=" + url.QueryEscape("http://127.0.0.1:41234/desktop-callback"))
	if err != nil {
		t.Fatal(err)
	}
	defer start.Body.Close()
	var started struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.NewDecoder(start.Body).Decode(&started); err != nil {
		t.Fatalf("decode the start: %v", err)
	}
	authURL, err := url.Parse(started.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := client.Get(f.srv.URL + "/api/auth/oidc/testsso/callback?code=c&state=" + url.QueryEscape(authURL.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Body.Close()
	if cb.StatusCode != http.StatusFound {
		t.Fatalf("the callback answered %d, want the 302 carrying the refusal", cb.StatusCode)
	}
	target, err := url.Parse(cb.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "127.0.0.1:41234" || target.Path != "/desktop-callback" {
		t.Fatalf("a refused desktop login redirected to %q, want the desktop's loopback — not the SPA login page", target)
	}
	if got := target.Query().Get("sso_error"); got != ssoErrPasswordChangeRequired {
		t.Fatalf("the loopback carries sso_error=%q, want %q", got, ssoErrPasswordChangeRequired)
	}
	if minted := rec.captured(); len(minted) != 0 {
		t.Fatalf("a refused login minted %d ticket(s), want none", len(minted))
	}
}

// TestDesktopSSO_AProviderErrorReachesTheLoopback is the refusal that never
// reaches the login at all — the IdP answered the callback with an error:
// the desktop flow's state is taken (single-use, the flow is over) and the
// refusal rides the loopback too.
func TestDesktopSSO_AProviderErrorReachesTheLoopback(t *testing.T) {
	f := newDerivationFixture(t)
	f.s.desktopTickets = &recordingTickets{Store: desktopsso.NewMemoryStore(time.Minute)}
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "desk-1", Email: "desk@example.org"})

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start, err := client.Get(f.srv.URL + "/api/auth/oidc/testsso/start?desktop=1&format=json&next=" + url.QueryEscape("http://127.0.0.1:41234/desktop-callback"))
	if err != nil {
		t.Fatal(err)
	}
	defer start.Body.Close()
	var started struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.NewDecoder(start.Body).Decode(&started); err != nil {
		t.Fatalf("decode the start: %v", err)
	}
	authURL, err := url.Parse(started.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	state := authURL.Query().Get("state")
	cb, err := client.Get(f.srv.URL + "/api/auth/oidc/testsso/callback?error=access_denied&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Body.Close()
	target, _ := url.Parse(cb.Header.Get("Location"))
	if cb.StatusCode != http.StatusFound || target.Host != "127.0.0.1:41234" {
		t.Fatalf("the provider error redirected to %q (%d), want the desktop's loopback", cb.Header.Get("Location"), cb.StatusCode)
	}
	if got := target.Query().Get("sso_error"); got != ssoErrProviderReturned {
		t.Fatalf("the loopback carries sso_error=%q, want %q", got, ssoErrProviderReturned)
	}
	// The flow's state went with the refusal: presenting it again knows
	// nothing of the desktop and lands on the SPA banner.
	again, err := client.Get(f.srv.URL + "/api/auth/oidc/testsso/callback?code=c&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	defer again.Body.Close()
	if loc := again.Header.Get("Location"); !strings.HasPrefix(loc, "/login?") {
		t.Fatalf("a replayed state answered %q, want the SPA login banner's state_expired", loc)
	}
}

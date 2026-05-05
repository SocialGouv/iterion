package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// newTestServerWithToken stands up a real *Server in front of httptest. We
// can't use the package's own ListenAndServe in unit tests so we wrap the
// configured handler directly.
func newTestServerWithToken(t *testing.T, token string) (*httptest.Server, *Server) {
	t.Helper()
	logger := iterlog.New(iterlog.LevelError, io.Discard)
	s := New(Config{Bind: "127.0.0.1", Port: 0, SessionToken: token}, logger)
	hs := httptest.NewServer(s.handler)
	t.Cleanup(hs.Close)
	return hs, s
}

func TestSessionToken_Empty_NoMiddleware(t *testing.T) {
	// When SessionToken is empty, every request must succeed without a
	// cookie — that's the byte-identical-to-CLI guarantee.
	hs, _ := newTestServerWithToken(t, "")
	resp, err := http.Get(hs.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("got 401 on /; middleware should be inert when token empty")
	}
}

func TestSessionToken_BootstrapSetsCookie(t *testing.T) {
	const tok = "test-token-value"
	hs, _ := newTestServerWithToken(t, tok)
	c := &http.Client{
		// Don't follow the 302 — we want to inspect Set-Cookie.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Get(hs.URL + "/?t=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", resp.StatusCode)
	}
	got := resp.Header.Get("Set-Cookie")
	if !strings.Contains(got, sessionCookieName+"="+tok) {
		t.Errorf("Set-Cookie missing token: %q", got)
	}
	if !strings.Contains(got, "HttpOnly") {
		t.Errorf("cookie missing HttpOnly: %q", got)
	}
}

func TestSessionToken_RejectsMissingCookie(t *testing.T) {
	hs, _ := newTestServerWithToken(t, "secret")
	resp, err := http.Get(hs.URL + "/api/effort-capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestSessionToken_AcceptsValidCookie(t *testing.T) {
	const tok = "secret"
	hs, _ := newTestServerWithToken(t, tok)
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/api/effort-capabilities", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("got 401 with valid cookie")
	}
}

func TestSessionToken_RejectsWrongCookie(t *testing.T) {
	hs, _ := newTestServerWithToken(t, "secret")
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/api/effort-capabilities", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "wrong"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// Cross-origin WebSocket connections (the desktop SPA dials the local server
// directly because Wails' AssetServer rejects WS upgrades) cannot share the
// HttpOnly cookie scoped to the local server's origin. The middleware must
// accept the same token via ?t=<token> on non-bootstrap paths so the dialer
// can authenticate without falling back to less-strict origin auth.
func TestSessionToken_AcceptsTokenQueryOnNonBootstrapPath(t *testing.T) {
	const tok = "secret"
	hs, _ := newTestServerWithToken(t, tok)
	resp, err := http.Get(hs.URL + "/api/effort-capabilities?t=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("got 401 with valid ?t=%q on /api/effort-capabilities", tok)
	}
	// Defense-in-depth: the query-param path must not set the cookie. The
	// caller is short-lived (a WS handshake) and shouldn't persist the token
	// in browser storage on a non-bootstrap origin.
	if got := resp.Header.Get("Set-Cookie"); got != "" {
		t.Errorf("non-bootstrap query auth must not set cookie, got %q", got)
	}
}

func TestSessionToken_RejectsWrongTokenQueryOnNonBootstrapPath(t *testing.T) {
	hs, _ := newTestServerWithToken(t, "secret")
	resp, err := http.Get(hs.URL + "/api/effort-capabilities?t=wrong")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 with wrong ?t=", resp.StatusCode)
	}
}

// The bootstrap GET / path should still set the cookie + redirect when ?t=
// matches — the legacy CLI flow that lands users at /?t=<token> must keep
// working byte-identically after the WS query-param extension.
func TestSessionToken_BootstrapSetsCookieEvenAfterQueryAuth(t *testing.T) {
	const tok = "secret"
	hs, _ := newTestServerWithToken(t, tok)
	c := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Get(hs.URL + "/?t=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Set-Cookie"); !strings.Contains(got, sessionCookieName+"="+tok) {
		t.Errorf("bootstrap path must still set cookie, got %q", got)
	}
}

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHostPrefixOnProductionCookieShape covers the configuration the cloud
// deployment actually runs (CookieSecure + no CookieDomain), which the older
// attribute test did not: its two cases both fall outside the prefix's terms,
// so they would keep passing however this resolved.
func TestHostPrefixOnProductionCookieShape(t *testing.T) {
	s := newAuthCookieServer(true, "")
	w := httptest.NewRecorder()
	s.setAuthCookies(w, "access-token", time.Now().Add(15*time.Minute), "refresh-token", time.Now().Add(24*time.Hour))
	cookies := w.Result().Cookies()

	access := findCookie(t, cookies, hostCookiePrefix+authCookieName)
	refresh := findCookie(t, cookies, hostCookiePrefix+refreshCookieName)

	// A browser DISCARDS a __Host- cookie that breaks any of these, so getting
	// one wrong means nobody can log in — assert all three on both cookies.
	for _, c := range []*http.Cookie{access, refresh} {
		if !c.Secure {
			t.Errorf("%s: missing Secure — a __Host- cookie without it is dropped", c.Name)
		}
		if c.Domain != "" {
			t.Errorf("%s: Domain = %q — a __Host- cookie must declare none", c.Name, c.Domain)
		}
		if c.Path != "/" {
			t.Errorf("%s: Path = %q — a __Host- cookie must be Path=/", c.Name, c.Path)
		}
		if !c.HttpOnly {
			t.Errorf("%s: missing HttpOnly", c.Name)
		}
	}

	// The bare names must NOT also be set: two live session cookies is the
	// ambiguity the prefix exists to remove.
	for _, c := range cookies {
		if c.Name == authCookieName || c.Name == refreshCookieName {
			t.Errorf("bare %q written alongside the prefixed cookie", c.Name)
		}
	}
}

// TestHostPrefixWithheldWhenItsTermsCannotBeMet locks the conditional. On a
// plaintext local studio, or a deployment that widens the cookie with
// CookieDomain, a __Host- cookie would be silently discarded by the browser —
// so those keep the bare names.
func TestHostPrefixWithheldWhenItsTermsCannotBeMet(t *testing.T) {
	cases := []struct {
		name   string
		secure bool
		domain string
	}{
		{"plaintext local studio", false, ""},
		{"explicit cookie domain", true, "studio.example"},
		{"neither", false, "studio.example"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newAuthCookieServer(c.secure, c.domain)
			w := httptest.NewRecorder()
			s.setAuthCookies(w, "a", time.Now().Add(time.Minute), "r", time.Now().Add(time.Hour))
			for _, got := range w.Result().Cookies() {
				if got.Name == hostCookiePrefix+authCookieName || got.Name == hostCookiePrefix+refreshCookieName {
					t.Fatalf("wrote %q under terms a browser would reject (secure=%v domain=%q)", got.Name, c.secure, c.domain)
				}
			}
			findCookie(t, w.Result().Cookies(), authCookieName)
			findCookie(t, w.Result().Cookies(), refreshCookieName)
		})
	}
}

// TestPrefixedCookieWinsOverATossedBareCookie is the attack this change is
// for: a sibling host under the shared registrable domain sets a bare
// `iterion_auth` on Domain=<parent>, hoping to pin the victim onto its own
// session. Reads must prefer the __Host- cookie, which no other host can write.
func TestPrefixedCookieWinsOverATossedBareCookie(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	// Order matters: the tossed cookie is sent FIRST, so a naive
	// r.Cookie(bare) would return the attacker's value.
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: "attacker-session"})
	r.AddCookie(&http.Cookie{Name: hostCookiePrefix + authCookieName, Value: "real-session"})

	if got := extractBearer(r); got != "real-session" {
		t.Fatalf("extractBearer = %q; want real-session — a tossed bare cookie shadowed the __Host- one", got)
	}
}

// TestLegacyCookieStillAccepted is the migration window: a session minted
// before this change carries only the bare name and must keep working until it
// expires, or the deploy signs everyone out.
func TestLegacyCookieStillAccepted(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: "legacy-session"})
	if got := extractBearer(r); got != "legacy-session" {
		t.Fatalf("extractBearer = %q; want legacy-session", got)
	}

	s := newAuthCookieServer(true, "")
	r2 := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
	r2.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "legacy-refresh"})
	if got := s.refreshTokenFromRequest(r2); got != "legacy-refresh" {
		t.Fatalf("refreshTokenFromRequest = %q; want legacy-refresh", got)
	}
}

// TestHostPrefixedSessionRoundTrip is the composition test: the unit tests
// above prove the write and the read separately, which would both stay green
// if the two disagreed on the name. This one registers through the real
// handler on a production-shaped server, takes whatever Set-Cookie came back,
// replays it on an authenticated endpoint, and requires it to authenticate.
func TestHostPrefixedSessionRoundTrip(t *testing.T) {
	srv := newSweepServer(t, func(c *Config) { c.CookieSecure = true })

	body := strings.NewReader(`{"email":"round@trip.test","password":"correct-horse-battery-staple"}`)
	reg := httptest.NewRequest(http.MethodPost, "/api/auth/register", body)
	reg.Host = "iterion.cloud"
	reg.Header.Set("Content-Type", "application/json")
	regRec := httptest.NewRecorder()
	srv.handler.ServeHTTP(regRec, reg)
	if regRec.Code != http.StatusOK && regRec.Code != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", regRec.Code, regRec.Body.String())
	}

	cookies := regRec.Result().Cookies()
	// On this shape the session must be the prefixed one.
	access := findCookie(t, cookies, hostCookiePrefix+authCookieName)

	me := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	me.Host = "iterion.cloud"
	me.AddCookie(&http.Cookie{Name: access.Name, Value: access.Value})
	meRec := httptest.NewRecorder()
	srv.handler.ServeHTTP(meRec, me)
	if meRec.Code != http.StatusOK {
		t.Fatalf("GET /api/auth/me with the minted %s: status %d, body %s — the cookie written is not the cookie read",
			access.Name, meRec.Code, meRec.Body.String())
	}
	if !strings.Contains(meRec.Body.String(), "round@trip.test") {
		t.Errorf("/api/auth/me did not return the session's own user: %s", meRec.Body.String())
	}
}

// TestClearAuthCookiesExpiresBothSpellings guards logout during the migration:
// a browser can hold the legacy cookie AND the prefixed one, so clearing only
// the name this build writes would leave the other live.
func TestClearAuthCookiesExpiresBothSpellings(t *testing.T) {
	s := newAuthCookieServer(true, "")
	w := httptest.NewRecorder()
	s.clearAuthCookies(w)

	cleared := map[string]*http.Cookie{}
	for _, c := range w.Result().Cookies() {
		cleared[c.Name] = c
	}
	for _, name := range []string{
		authCookieName,
		refreshCookieName,
		hostCookiePrefix + authCookieName,
		hostCookiePrefix + refreshCookieName,
	} {
		c, ok := cleared[name]
		if !ok {
			t.Errorf("%q not expired on logout", name)
			continue
		}
		if c.MaxAge != -1 {
			t.Errorf("%q MaxAge = %d; want -1", name, c.MaxAge)
		}
		// A __Host- deletion is only honoured on the same terms as its write.
		if name == hostCookiePrefix+authCookieName || name == hostCookiePrefix+refreshCookieName {
			if !c.Secure || c.Domain != "" || c.Path != "/" {
				t.Errorf("%q deletion breaks the prefix terms (secure=%v domain=%q path=%q) and would be ignored",
					name, c.Secure, c.Domain, c.Path)
			}
		}
	}
}

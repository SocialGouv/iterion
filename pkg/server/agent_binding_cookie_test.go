package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The OIDC and forge connect flows each carry a per-flow, HttpOnly
// "agent-binding" cookie, compared at the callback against the value stored
// with the pending state. That is the login-CSRF guard of RFC 9700 §4.7.1: it
// is what stops an attacker completing an authorization flow in a victim's
// browser and landing them on the attacker's account.
//
// The code used to justify keeping it bare-named with "a cross-site script …
// can't set a cookie for iterion's origin (same-origin policy)". That is false
// on a host under a shared registrable domain — a sibling sets Domain=<parent>
// and the browser sends it alongside ours — which is the premise of this whole
// change. These cookies are the same class as the session ones and were fixed
// with them.

func TestAgentBindingCookiesCarryTheHostPrefix(t *testing.T) {
	s := newAuthCookieServer(true, "")

	t.Run("forge", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.setForgeAgentBindingCookie(w, "binding-value")
		c := findCookie(t, w.Result().Cookies(), hostCookiePrefix+forgeAgentBindingCookie)
		if c.Path != "/" || !c.Secure || c.Domain != "" {
			t.Errorf("%s breaks the prefix terms (path=%q secure=%v domain=%q) and a browser would discard it",
				c.Name, c.Path, c.Secure, c.Domain)
		}
		if !c.HttpOnly {
			t.Errorf("%s lost HttpOnly", c.Name)
		}
	})

	// The OIDC cookie is written inline in handleOIDCStart rather than through
	// a helper, so its name and path are asserted at the two functions that
	// compute them — the same ones the forge helper above goes through.
	t.Run("oidc name and path", func(t *testing.T) {
		if got := s.authCookieWriteName(oidcAgentBindingCookie); got != hostCookiePrefix+oidcAgentBindingCookie {
			t.Errorf("write name = %q; want the prefixed form", got)
		}
		if got := s.agentBindingCookiePath("/api/auth/oidc/"); got != "/" {
			t.Errorf("path = %q; want / — __Host- mandates it and a browser discards the cookie otherwise", got)
		}
	})

	t.Run("withheld where the terms cannot be met", func(t *testing.T) {
		plain := newAuthCookieServer(false, "")
		w := httptest.NewRecorder()
		plain.setForgeAgentBindingCookie(w, "binding-value")
		c := findCookie(t, w.Result().Cookies(), forgeAgentBindingCookie)
		if c.Path != "/api/forge/" {
			t.Errorf("path = %q; want the narrow /api/forge/ when unprefixed", c.Path)
		}
	})
}

// TestTossedAgentBindingCookieIsRefused is the guard itself: on a deployment
// that writes the prefix, a bare cookie — the only kind a sibling host can
// write — must not satisfy the binding check.
func TestTossedAgentBindingCookieIsRefused(t *testing.T) {
	s := newAuthCookieServer(true, "")
	for _, name := range []string{oidcAgentBindingCookie, forgeAgentBindingCookie} {
		r := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/x/callback", nil)
		r.AddCookie(&http.Cookie{Name: name, Value: "attacker-binding"})
		if got := s.sessionCookie(r, name, false); got != "" {
			t.Errorf("%s: a tossed bare cookie read back as %q — the login-CSRF guard is defeated", name, got)
		}
	}
}

// TestAgentBindingCookiesClearBothSpellings keeps the single-use property
// through the migration: a flow started before the deploy holds the bare name,
// and a binding cookie that outlives its flow is a replayable one.
func TestAgentBindingCookiesClearBothSpellings(t *testing.T) {
	cases := []struct {
		name  string
		clear func(http.ResponseWriter, string, bool)
		base  string
	}{
		{"oidc", clearOIDCAgentBindingCookie, oidcAgentBindingCookie},
		{"forge", clearForgeAgentBindingCookie, forgeAgentBindingCookie},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c.clear(w, "", true)
			got := map[string]bool{}
			for _, ck := range w.Result().Cookies() {
				if ck.MaxAge == -1 {
					got[ck.Name] = true
				}
			}
			for _, want := range []string{c.base, hostCookiePrefix + c.base} {
				if !got[want] {
					t.Errorf("%q not expired; a stale binding cookie stays replayable", want)
				}
			}
		})
	}
}

package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

func TestMapAuthErrorStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"invalid credentials", auth.ErrInvalidCredentials, http.StatusUnauthorized},
		{"account disabled", auth.ErrAccountDisabled, http.StatusUnauthorized},
		{"session revoked", auth.ErrSessionRevoked, http.StatusUnauthorized},
		{"session expired", auth.ErrSessionExpired, http.StatusUnauthorized},
		{"token expired", auth.ErrTokenExpired, http.StatusUnauthorized},
		{"token invalid", auth.ErrTokenInvalid, http.StatusUnauthorized},
		{"token revoked", auth.ErrTokenRevoked, http.StatusUnauthorized},
		{"signup closed", auth.ErrSignupClosed, http.StatusBadRequest},
		{"invitation mismatch", auth.ErrInvitationMismatch, http.StatusBadRequest},
		{"password weak", auth.ErrPasswordWeak, http.StatusBadRequest},
		{"link requires consent", auth.ErrLinkRequiresConsent, http.StatusConflict},
		{"invitation not found", auth.ErrInvitationNotFound, http.StatusNotFound},
		{"team not found", auth.ErrTeamNotFound, http.StatusNotFound},
		{"identity not found", identity.ErrNotFound, http.StatusNotFound},
		{"email taken", identity.ErrEmailAlreadyTaken, http.StatusConflict},
		{"slug taken", identity.ErrSlugAlreadyTaken, http.StatusConflict},
		{"invitation used", identity.ErrInvitationUsed, http.StatusConflict},
		{"invitation expired", identity.ErrInvitationExpired, http.StatusGone},
		{"not a member", auth.ErrNotAMember, http.StatusForbidden},
		{"password change required", auth.ErrPasswordChangeRequired, http.StatusForbidden},
		{"unrecognised", errors.New("something else"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mapAuthErrorStatus(c.err); got != c.want {
				t.Errorf("mapAuthErrorStatus(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

func TestMapAuthErrorStatusRespectsWrap(t *testing.T) {
	wrapped := errors.New("login failed: " + auth.ErrInvalidCredentials.Error())
	// errors.New("…") loses identity; mapAuthErrorStatus falls back to 500.
	if got := mapAuthErrorStatus(wrapped); got != http.StatusInternalServerError {
		t.Errorf("plain string-wrap should fall through; got %d", got)
	}
	// %w wrap retains identity.
	wWrap := errors.Join(errors.New("login failed"), auth.ErrTokenExpired)
	if got := mapAuthErrorStatus(wWrap); got != http.StatusUnauthorized {
		t.Errorf("errors.Join with sentinel should map; got %d", got)
	}
}

func TestIsBrowserClient(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"no headers (CLI)", nil, false},
		{"sec-fetch-site only", map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"sec-fetch-mode only", map[string]string{"Sec-Fetch-Mode": "cors"}, true},
		{"origin only", map[string]string{"Origin": "https://studio.example"}, true},
		{"both fetch headers", map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors"}, true},
		{"user-agent alone is not enough", map[string]string{"User-Agent": "Mozilla/5.0"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/login", nil)
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			if got := isBrowserClient(r); got != c.want {
				t.Errorf("isBrowserClient(%v) = %v, want %v", c.headers, got, c.want)
			}
		})
	}
}

func TestSafeNext(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"plain path", "/dashboard", "/dashboard"},
		{"path with query", "/runs?id=abc", "/runs?id=abc"},
		{"absolute url", "https://evil.example/x", ""},
		{"scheme-relative", "//evil.example/x", ""},
		{"protocol-relative path", "//evil/x", ""},
		{"non-root path", "dashboard", ""},
		{"trailing slash root", "/", "/"},
		{"backslash leading", "/\\evil.com", ""},
		{"backslash scheme-relative", "\\\\evil.com", ""},
		{"backslash mixed", "/\\/evil.com", ""},
		{"backslash in path", "/foo\\bar", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := safeNext(c.in); got != c.want {
				t.Errorf("safeNext(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func newAuthCookieServer(secure bool, domain string) *Server {
	return &Server{
		cfg: Config{
			CookieSecure: secure,
			CookieDomain: domain,
		},
	}
}

func findCookie(t *testing.T, cookies []*http.Cookie, name string) *http.Cookie {
	t.Helper()
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("cookie %q not present in %v", name, cookies)
	return nil
}

func TestSetAuthCookiesAttributes(t *testing.T) {
	exp := time.Now().Add(15 * time.Minute)
	rexp := time.Now().Add(7 * 24 * time.Hour)

	t.Run("secure cookies with domain", func(t *testing.T) {
		s := newAuthCookieServer(true, "studio.example")
		w := httptest.NewRecorder()
		s.setAuthCookies(w, "access-token", exp, "refresh-token", rexp)

		cookies := w.Result().Cookies()
		access := findCookie(t, cookies, authCookieName)
		if access.Value != "access-token" {
			t.Errorf("access value = %q, want access-token", access.Value)
		}
		if access.Path != "/" {
			t.Errorf("access Path = %q, want /", access.Path)
		}
		if access.Domain != "studio.example" {
			t.Errorf("access Domain = %q, want studio.example", access.Domain)
		}
		if !access.HttpOnly {
			t.Error("access cookie missing HttpOnly")
		}
		if !access.Secure {
			t.Error("access cookie missing Secure")
		}
		if access.SameSite != http.SameSiteLaxMode {
			t.Errorf("access SameSite = %v, want Lax", access.SameSite)
		}

		refresh := findCookie(t, cookies, refreshCookieName)
		if refresh.Path != "/api/auth" {
			t.Errorf("refresh Path = %q, want /api/auth", refresh.Path)
		}
		if !refresh.HttpOnly || !refresh.Secure {
			t.Error("refresh cookie missing HttpOnly or Secure")
		}
		if refresh.SameSite != http.SameSiteLaxMode {
			t.Errorf("refresh SameSite = %v, want Lax", refresh.SameSite)
		}
	})

	t.Run("non-secure local mode", func(t *testing.T) {
		s := newAuthCookieServer(false, "")
		w := httptest.NewRecorder()
		s.setAuthCookies(w, "access-token", exp, "refresh-token", rexp)
		for _, c := range w.Result().Cookies() {
			if c.Secure {
				t.Errorf("cookie %q must not be Secure when CookieSecure=false", c.Name)
			}
			if c.Domain != "" {
				t.Errorf("cookie %q Domain = %q, want empty", c.Name, c.Domain)
			}
		}
	})

	t.Run("empty access token skips that cookie", func(t *testing.T) {
		s := newAuthCookieServer(false, "")
		w := httptest.NewRecorder()
		s.setAuthCookies(w, "", exp, "refresh-token", rexp)

		cookies := w.Result().Cookies()
		for _, c := range cookies {
			if c.Name == authCookieName {
				t.Error("access cookie should not be set when token is empty")
			}
		}
		// Refresh should still be present.
		findCookie(t, cookies, refreshCookieName)
	})

	t.Run("whitespace-only access token is treated as empty", func(t *testing.T) {
		s := newAuthCookieServer(false, "")
		w := httptest.NewRecorder()
		s.setAuthCookies(w, "   ", exp, "refresh-token", rexp)
		for _, c := range w.Result().Cookies() {
			if c.Name == authCookieName {
				t.Error("access cookie should not be set when token is whitespace-only")
			}
		}
	})

	t.Run("empty refresh token skips that cookie", func(t *testing.T) {
		s := newAuthCookieServer(false, "")
		w := httptest.NewRecorder()
		s.setAuthCookies(w, "access-token", exp, "", rexp)
		for _, c := range w.Result().Cookies() {
			if c.Name == refreshCookieName {
				t.Error("refresh cookie should not be set when token is empty")
			}
		}
	})
}

func TestClearAuthCookies(t *testing.T) {
	s := newAuthCookieServer(true, "studio.example")
	w := httptest.NewRecorder()
	s.clearAuthCookies(w)

	cookies := w.Result().Cookies()
	access := findCookie(t, cookies, authCookieName)
	refresh := findCookie(t, cookies, refreshCookieName)

	for _, c := range []*http.Cookie{access, refresh} {
		if c.MaxAge >= 0 {
			t.Errorf("cookie %q MaxAge = %d, want < 0", c.Name, c.MaxAge)
		}
		if c.Value != "" {
			t.Errorf("cookie %q value = %q, want empty", c.Name, c.Value)
		}
		if !c.HttpOnly || !c.Secure {
			t.Errorf("cookie %q missing HttpOnly or Secure", c.Name)
		}
		if c.Domain != "studio.example" {
			t.Errorf("cookie %q Domain = %q, want studio.example", c.Name, c.Domain)
		}
	}
	// Path-scoping must match the set path so the browser clears the
	// right cookie (a wrong Path is silently ignored by browsers).
	if access.Path != "/" {
		t.Errorf("access clear Path = %q, want /", access.Path)
	}
	if refresh.Path != "/api/auth" {
		t.Errorf("refresh clear Path = %q, want /api/auth", refresh.Path)
	}
}

func TestRefreshTokenFromRequest(t *testing.T) {
	s := newAuthCookieServer(false, "")

	t.Run("from cookie", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
		r.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "cookie-refresh"})
		if got := s.refreshTokenFromRequest(r); got != "cookie-refresh" {
			t.Errorf("got %q, want cookie-refresh", got)
		}
	})

	t.Run("from header fallback", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
		r.Header.Set("X-Iterion-Refresh", "header-refresh")
		if got := s.refreshTokenFromRequest(r); got != "header-refresh" {
			t.Errorf("got %q, want header-refresh", got)
		}
	})

	t.Run("cookie wins over header", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
		r.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "cookie-refresh"})
		r.Header.Set("X-Iterion-Refresh", "header-refresh")
		if got := s.refreshTokenFromRequest(r); got != "cookie-refresh" {
			t.Errorf("cookie should win; got %q", got)
		}
	})

	t.Run("nothing", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
		if got := s.refreshTokenFromRequest(r); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestPeerIsTrusted(t *testing.T) {
	t.Run("no trusted CIDRs configured", func(t *testing.T) {
		s := &Server{cfg: Config{}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "127.0.0.1:54321"
		if s.peerIsTrusted(r) {
			t.Error("peer should not be trusted when no CIDRs configured")
		}
	})

	t.Run("ip inside CIDR", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.2.3:443"
		if !s.peerIsTrusted(r) {
			t.Error("10.1.2.3 should be trusted by 10.0.0.0/8")
		}
	})

	t.Run("ip outside CIDR", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "192.168.1.5:443"
		if s.peerIsTrusted(r) {
			t.Error("192.168.1.5 should not be trusted by 10.0.0.0/8")
		}
	})

	t.Run("garbled CIDR is skipped, not panic", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"not-a-cidr", "10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.0.0.1:443"
		if !s.peerIsTrusted(r) {
			t.Error("valid CIDR should still match when paired with garbled one")
		}
	})

	t.Run("missing port", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.0.0.1" // no :port
		if !s.peerIsTrusted(r) {
			t.Error("portless RemoteAddr should still match")
		}
	})

	t.Run("nil receiver", func(t *testing.T) {
		var s *Server
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if s.peerIsTrusted(r) {
			t.Error("nil Server should not trust")
		}
	})
}

func TestClientIP(t *testing.T) {
	// The ephemeral source port is never part of the identity (#2673):
	// with it, every TCP connection of one client is a different client
	// and buys its own rate-limit bucket.
	t.Run("untrusted peer: the socket host wins, without its port", func(t *testing.T) {
		s := &Server{cfg: Config{}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "203.0.113.5:54321"
		r.Header.Set("X-Forwarded-For", "1.2.3.4")
		if got := s.clientIP(r); got != "203.0.113.5" {
			t.Errorf("got %q, want the socket host 203.0.113.5 without its port", got)
		}
	})

	// A dual-stack listener spells an IPv4 client's socket
	// "::ffff:1.2.3.4": one host must not be two identities, and two
	// identities are two rate-limit buckets.
	t.Run("untrusted peer: an IPv4-mapped socket is the IPv4 address", func(t *testing.T) {
		s := &Server{cfg: Config{}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "[::ffff:203.0.113.5]:54321"
		if got := s.clientIP(r); got != "203.0.113.5" {
			t.Errorf("got %q, want 203.0.113.5", got)
		}
	})

	// A peer address the kernel did not spell as an IP is still that peer:
	// it must not collapse to the empty identity every client would share.
	t.Run("untrusted peer: a socket that is not an IP stays itself", func(t *testing.T) {
		s := &Server{cfg: Config{}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "@"
		if got := s.clientIP(r); got != "@" {
			t.Errorf("got %q, want the peer as given", got)
		}
	})

	// And a peer address the transport reported as NOTHING is not the
	// empty identity either: net/http's listener always sets one, so this
	// is an embedder's own transport or a handler called directly, and ""
	// would be one bucket and one blank audit row shared by every such
	// request. A name no address can be confused with says what happened.
	t.Run("no peer address at all is a named identity, not the empty one", func(t *testing.T) {
		for name, prepare := range map[string]func(*http.Request){
			"no forwarded header":                func(*http.Request) {},
			"a forwarded header no peer vouches": func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.5") },
		} {
			s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = ""
			prepare(r)
			got := s.clientIP(r)
			if got == "" {
				t.Errorf("%s: the identity is the empty string — every peerless request shares one bucket and writes a blank audit row", name)
			}
			if got != unknownPeer {
				t.Errorf("%s: the identity is %q, want %q", name, got, unknownPeer)
			}
			if canonicalIP(got) != "" {
				t.Errorf("%s: the sentinel %q parses as an address, so it can collide with a real client", name, got)
			}
		}
	})

	// The chain is client-writable up to MaxHeaderBytes and the walk runs
	// BEFORE the limiter has a key to refuse the request with, so only the
	// hops nearest us are read. A client that pads its way past the bound
	// lands on the proxy's own bucket — less reach, not more.
	t.Run("trusted peer: a chain longer than the hop bound is read from our end", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		chain := []string{"203.0.113.42"}
		for i := 0; i < maxForwardedHops+1; i++ {
			chain = append(chain, "10.0.0.7")
		}
		r.Header.Set("X-Forwarded-For", strings.Join(chain, ", "))
		if got := s.clientIP(r); got != "10.0.0.7" {
			t.Errorf("got %q, want 10.0.0.7 — only the %d hops nearest us are read, and they are all ours", got, maxForwardedHops)
		}
	})

	t.Run("untrusted peer: an IPv6 socket keeps its address, loses its port", func(t *testing.T) {
		s := &Server{cfg: Config{}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "[2001:db8::17]:443"
		if got := s.clientIP(r); got != "2001:db8::17" {
			t.Errorf("got %q, want 2001:db8::17", got)
		}
	})

	t.Run("trusted peer with X-Forwarded-For (single)", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", "203.0.113.5")
		if got := s.clientIP(r); got != "203.0.113.5" {
			t.Errorf("got %q, want 203.0.113.5", got)
		}
	})

	// Three entries, so three APPENDING hops of ours: the ingress wrote
	// 10.0.0.8, the proxy at 10.0.0.8 wrote 10.0.0.7, and the one at
	// 10.0.0.7 wrote the client. The COUNT is what says so — nothing in
	// the chain's content tells a proxy's entry from a client's, which is
	// why the reader walks exactly as far as the operator declares.
	t.Run("trusted peer with X-Forwarded-For (chain)", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, TrustedProxyHops: 3}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.7, 10.0.0.8")
		if got := s.clientIP(r); got != "203.0.113.5" {
			t.Errorf("got %q, want the entry the outermost of the three hops saw, 203.0.113.5", got)
		}
	})

	// The same chain with the default count reaches only the hop nearest
	// us: an undeclared second hop costs per-client keying, it never
	// hands the identity to what a client wrote.
	t.Run("trusted peer: an undeclared hop stops the walk at the nearest one", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.7, 10.0.0.8")
		if got := s.clientIP(r); got != "10.0.0.8" {
			t.Errorf("got %q, want 10.0.0.8 — one declared hop reaches one entry", got)
		}
	})

	// Only the header the OPERATOR declared is read. Reading whichever
	// mechanism a request happened to carry would hand the choice to the
	// client, which is the whole defect: an X-Real-IP nobody declared is
	// a header a client can send.
	t.Run("trusted peer: an undeclared mechanism is not read", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Real-IP", "203.0.113.99")
		if got := s.clientIP(r); got != "10.1.1.1" {
			t.Errorf("got %q, want the peer host 10.1.1.1: X-Forwarded-For is the declared header and the request carries none", got)
		}
	})

	t.Run("trusted peer: the declared X-Real-IP is read", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: "X-Real-IP"}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Real-IP", "203.0.113.99")
		if got := s.clientIP(r); got != "203.0.113.99" {
			t.Errorf("got %q, want 203.0.113.99", got)
		}
	})

	// A CDN names the client in a header of its own. Refusing a name
	// outside a closed set of three would be a bound with no override.
	t.Run("trusted peer: a declared CDN header is read as itself", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: "CF-Connecting-IP"}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("CF-Connecting-IP", "203.0.113.44")
		r.Header.Set("X-Forwarded-For", "9.9.9.9")
		if got := s.clientIP(r); got != "203.0.113.44" {
			t.Errorf("got %q, want 203.0.113.44 — the declared header, not the one the client wrote", got)
		}
	})

	// The operator's spelling is not the canonical one Header.Values
	// looks a name up by ("x-real-ip" canonicalises to "X-Real-Ip"), and
	// an identity that depended on the case of a config value would be
	// read from no header at all.
	t.Run("trusted peer: the declared name is case-insensitive", func(t *testing.T) {
		for _, spelling := range []string{"x-real-ip", "X-REAL-IP", "X-Real-IP"} {
			s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: spelling}}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "10.1.1.1:443"
			r.Header.Set("X-Real-IP", "203.0.113.99")
			if got := s.clientIP(r); got != "203.0.113.99" {
				t.Errorf("declared %q: got %q, want 203.0.113.99", spelling, got)
			}
		}
	})

	t.Run("trusted peer without forward headers", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		if got := s.clientIP(r); got != "10.1.1.1" {
			t.Errorf("got %q, want the peer host 10.1.1.1 without its port", got)
		}
	})

	// A proxy that writes the port into its forwarded entry would hand
	// the limiter a per-connection key again.
	t.Run("trusted peer: a forwarded entry loses its port too", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, TrustedProxyHops: 2}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", "203.0.113.5:61234, 10.0.0.7")
		if got := s.clientIP(r); got != "203.0.113.5" {
			t.Errorf("got %q, want 203.0.113.5", got)
		}
	})

	// The chain is read right to left as far as the declared hops: with
	// two appending proxies in front, the second entry from the right is
	// the address the outer one saw, and anything further left is what
	// the client wrote.
	t.Run("trusted peer: the rightmost entry outside the ranges is the client", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, TrustedProxyHops: 2}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", "9.9.9.9, 203.0.113.77, 10.0.0.7")
		if got := s.clientIP(r); got != "203.0.113.77" {
			t.Errorf("got %q, want 203.0.113.77 — 9.9.9.9 is whatever the client wrote", got)
		}
	})

	// A client can send its own header line; the proxy adds a second.
	// Only the whole list is the chain.
	t.Run("trusted peer: two X-Forwarded-For header lines are one chain", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, TrustedProxyHops: 2}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Add("X-Forwarded-For", "9.9.9.9")
		r.Header.Add("X-Forwarded-For", "203.0.113.77, 10.0.0.7")
		if got := s.clientIP(r); got != "203.0.113.77" {
			t.Errorf("got %q, want 203.0.113.77", got)
		}
	})

	// When every hop is inside the ranges the answer is the RIGHTMOST of
	// them — the address the last appending proxy actually saw. The
	// leftmost is the slot the client writes into, so answering with it
	// would hand the identity back to the client; the leading empty entry
	// is dropped, not carried, since every client would share it.
	t.Run("trusted peer: a chain of our own proxies answers with its hop nearest us", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", ", 10.0.0.5, 10.0.0.7")
		if got := s.clientIP(r); got != "10.0.0.7" {
			t.Errorf("got %q, want 10.0.0.7 — the rightmost hop; 10.0.0.5 is one entry further into what the client wrote", got)
		}
	})

	// The fallback must not hand the identity to a client that sits
	// inside a configured range: with the chart's private-range default,
	// every LAN client and every in-cluster pod does. Rotating a forged
	// entry must leave the identity fixed, or #2673's own defect comes
	// back through the header instead of the source port — and the
	// forged value's own RANGE must not matter. A walk that skipped
	// every in-range hop answered with an out-of-range forgery, because
	// skipping the appended hop (the in-range client's own address) led
	// it straight back into the half of the chain the client wrote.
	t.Run("trusted peer: an in-range client's forged entry never becomes the identity", func(t *testing.T) {
		seen := map[string]bool{}
		for _, forged := range []string{
			"10.9.9.1", "10.9.9.2", "10.9.9.3", // in range, like the client itself
			"203.0.113.77", "198.51.100.5", "8.8.8.8", // outside every range
		} {
			s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "10.1.1.1:443"
			// An appending proxy writes what the client sent, then the
			// address it saw — here an in-range client of its own.
			r.Header.Set("X-Forwarded-For", forged+", 10.2.2.2")
			got := s.clientIP(r)
			if got != "10.2.2.2" {
				t.Errorf("forged %q: got %q, want 10.2.2.2 — the hop the proxy saw", forged, got)
			}
			seen[got] = true
		}
		if len(seen) != 1 {
			t.Errorf("six forged entries produced %d identities %v, want 1: each one is a fresh rate-limit bucket", len(seen), seen)
		}
	})

	// The same class through the declared RFC 7239 mechanism: the walk is
	// one function, so a chain parsed from `Forwarded` must stop at the
	// same hop.
	t.Run("trusted peer: the RFC 7239 chain stops at the appended hop too", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: "Forwarded"}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("Forwarded", "for=203.0.113.77,for=10.2.2.2")
		if got := s.clientIP(r); got != "10.2.2.2" {
			t.Errorf("got %q, want 10.2.2.2 — the hop the proxy appended", got)
		}
	})

	// No number of in-range entries a client writes moves the bound: the
	// walk counts OUR hops, and with one appending proxy in front the
	// first address from the right is the answer.
	t.Run("trusted peer: padding the chain with in-range entries moves nothing", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", "203.0.113.77, 10.0.0.1, 172.16.0.1, 192.168.0.1, 10.2.2.2")
		if got := s.clientIP(r); got != "10.2.2.2" {
			t.Errorf("got %q, want 10.2.2.2 — four in-range entries the client wrote skipped the bound", got)
		}
	})

	// The override is load-bearing, not decoration: a CDN edge that
	// appends in front of the ingress writes a SECOND entry of ours, and
	// the client the operator wants is then the second from the right.
	// Without the knob, listing the CDN's ranges would no longer reach
	// the client.
	t.Run("trusted peer: two declared hops answer with the entry the outer proxy saw", func(t *testing.T) {
		cfg := Config{TrustedProxyCIDRs: []string{"10.0.0.0/8", "198.51.100.0/24"}, TrustedProxyHops: 2}
		send := func(chain string) string {
			s := &Server{cfg: cfg}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "10.1.1.1:443" // the ingress
			r.Header.Set("X-Forwarded-For", chain)
			return s.clientIP(r)
		}
		// forged, the client as the CDN saw it, the CDN edge as the
		// ingress saw it.
		if got := send("9.9.9.9, 203.0.113.5, 198.51.100.7"); got != "203.0.113.5" {
			t.Errorf("got %q, want 203.0.113.5 — the address the outer declared hop saw", got)
		}
		// And an in-range client behind the same two hops is still
		// unforgeable: its own address is the second from the right.
		if got := send("9.9.9.9, 10.2.2.2, 198.51.100.7"); got != "10.2.2.2" {
			t.Errorf("got %q, want 10.2.2.2", got)
		}
	})

	// The residual, pinned so it is not denied: the subtest above is the
	// APPENDED shape, which is the deployment the ranges are configured
	// for. A peer that reaches the port DIRECTLY with nothing in front
	// wrote the whole chain itself, so its rightmost entry is
	// client-chosen too — it names any address it likes, including an
	// in-range victim's, and rotating the value moves the key. Trusting
	// a range is trusting what every address inside it says; the chart
	// default makes that every in-cluster pod, which is why the ranges
	// belong to the ingress controller and `networkPolicy.enabled` keeps
	// the port out of everything else's reach (#2804).
	t.Run("trusted peer: a direct in-range peer with nothing appended still names its own chain", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8", "192.168.0.0/16"}}}
		send := func(forged string) string {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "10.1.2.3:51000" // an in-cluster pod, no proxy in front
			r.Header.Set("X-Forwarded-For", forged)
			return s.clientIP(r)
		}
		if got := send("10.9.9.1"); got != "10.9.9.1" {
			t.Errorf("got %q, want the forged 10.9.9.1 — the documented residual of trusting a range, and the comment on forwardedClientIP says so", got)
		}
		if got := send("192.168.5.10"); got != "192.168.5.10" {
			t.Errorf("got %q, want the forged 192.168.5.10 — the same peer names an arbitrary in-range victim", got)
		}
	})

	// An identity is one spelling of one address: cycling spellings must
	// not buy fresh buckets, and a value that is not an address at all
	// must never become the audit IP or reach a log line.
	t.Run("trusted peer: a forwarded address is canonicalised", func(t *testing.T) {
		for _, spelling := range []string{"2001:0DB8:0000::0017", "[2001:db8::17]", "2001:db8::17"} {
			s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "10.1.1.1:443"
			r.Header.Set("X-Forwarded-For", spelling)
			if got := s.clientIP(r); got != "2001:db8::17" {
				t.Errorf("%s: got %q, want 2001:db8::17", spelling, got)
			}
		}
	})

	t.Run("trusted peer: an IPv4-mapped form answers as the IPv4 address", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", "::ffff:203.0.113.5")
		if got := s.clientIP(r); got != "203.0.113.5" {
			t.Errorf("got %q, want 203.0.113.5 — one host, one identity", got)
		}
	})

	t.Run("trusted peer: a forwarded value that is not an address falls through", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", "not-an-ip\r\nX-Injected: 1")
		if got := s.clientIP(r); got != "10.1.1.1" {
			t.Errorf("got %q, want the peer host 10.1.1.1: a non-address must not become the identity", got)
		}
	})

	// A bracketed IPv6 literal with no port is what a proxy writes into
	// X-Forwarded-For; the brackets are not part of the address.
	t.Run("trusted peer: a bracketed IPv6 entry without a port loses its brackets", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", "[2001:db8::99]")
		if got := s.clientIP(r); got != "2001:db8::99" {
			t.Errorf("got %q, want 2001:db8::99", got)
		}
	})

	t.Run("trusted peer: the declared RFC 7239 Forwarded is read", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: "Forwarded"}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("Forwarded", `for=203.0.113.60;proto=https;by=10.1.1.1`)
		if got := s.clientIP(r); got != "203.0.113.60" {
			t.Errorf("got %q, want 203.0.113.60", got)
		}
	})

	// The RFC quotes a for= value that carries a port, and an IPv6
	// address brings both brackets and colons: the element separator
	// must not be looked for inside the quotes.
	t.Run("trusted peer: Forwarded with a quoted IPv6 client and a proxy chain", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: "Forwarded", TrustedProxyHops: 2}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("Forwarded", `For="[2001:db8:cafe::17]:4711";proto=https, for=10.0.0.7`)
		if got := s.clientIP(r); got != "2001:db8:cafe::17" {
			t.Errorf("got %q, want 2001:db8:cafe::17", got)
		}
	})

	t.Run("trusted peer: an obfuscated Forwarded identifier names no client", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, ForwardedHeader: "Forwarded"}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("Forwarded", "for=_hidden;proto=https")
		// An undeclared mechanism is not a fallback: this X-Real-IP is as
		// client-writable as the obfuscated value beside it.
		r.Header.Set("X-Real-IP", "203.0.113.99")
		if got := s.clientIP(r); got != "10.1.1.1" {
			t.Errorf("got %q, want the peer host 10.1.1.1: the declared header named no client and nothing else is read", got)
		}
	})

	// An empty entry names nobody: it is dropped, and the real entry
	// beside it is the client.
	t.Run("trusted peer: an empty leftmost X-Forwarded-For entry is dropped", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", ", 203.0.113.5")
		if got := s.clientIP(r); got != "203.0.113.5" {
			t.Errorf("got %q, want 203.0.113.5", got)
		}
	})

	t.Run("trusted peer: a header of separators alone falls back to the peer", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", " , ")
		if got := s.clientIP(r); got != "10.1.1.1" {
			t.Errorf("got %q, want the peer host 10.1.1.1", got)
		}
	})

	t.Run("trusted peer: a header carrying only spaces falls through", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", "   ")
		r.Header.Set("X-Real-IP", "  ") // undeclared, so read by nothing
		if got := s.clientIP(r); got != "10.1.1.1" {
			t.Errorf("got %q, want the peer host 10.1.1.1", got)
		}
	})
}

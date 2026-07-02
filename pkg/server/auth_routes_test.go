package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
	t.Run("untrusted peer: RemoteAddr wins", func(t *testing.T) {
		s := &Server{cfg: Config{}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "203.0.113.5:54321"
		r.Header.Set("X-Forwarded-For", "1.2.3.4")
		if got := s.clientIP(r); got != "203.0.113.5:54321" {
			t.Errorf("got %q, want RemoteAddr", got)
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

	t.Run("trusted peer with X-Forwarded-For (chain)", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.7, 10.0.0.8")
		if got := s.clientIP(r); got != "203.0.113.5" {
			t.Errorf("got %q, want client (leftmost) 203.0.113.5", got)
		}
	})

	t.Run("trusted peer with X-Real-IP fallback", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		r.Header.Set("X-Real-IP", "203.0.113.99")
		if got := s.clientIP(r); got != "203.0.113.99" {
			t.Errorf("got %q, want 203.0.113.99", got)
		}
	})

	t.Run("trusted peer without forward headers", func(t *testing.T) {
		s := &Server{cfg: Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.1.1.1:443"
		if got := s.clientIP(r); got != "10.1.1.1:443" {
			t.Errorf("got %q, want RemoteAddr", got)
		}
	})
}

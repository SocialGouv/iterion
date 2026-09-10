package server

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
)

// usesHostPrefix reports whether this deployment can carry __Host- cookies.
// The prefix is only honoured by a browser when the cookie is Secure, has
// Path=/ and declares NO Domain — so a plaintext local studio, or a
// deployment that deliberately widens the cookie with CookieDomain, keeps
// the bare names. Emitting a __Host- cookie there would not be "less
// secure", it would be silently DISCARDED, i.e. nobody could log in.
func (s *Server) usesHostPrefix() bool {
	return s.cfg.CookieSecure && s.cfg.CookieDomain == ""
}

// authCookieWriteName is the name this deployment SETS for a session cookie.
// Reads are governed by sessionCookie, which is deliberately NOT symmetric.
func (s *Server) authCookieWriteName(base string) string {
	if s.usesHostPrefix() {
		return hostCookiePrefix + base
	}
	return base
}

// sessionCookie reads a session cookie under this deployment's own naming.
//
// A sibling host under the same registrable domain can set a cookie with
// Domain=<shared parent> and the bare name, which the browser then sends
// alongside ours ("cookie tossing") — enough to pin a victim onto an
// attacker's session. A __Host- cookie cannot be written that way, so where
// this deployment writes the prefix, the prefixed name is what counts.
//
// Two rules, each paying for a way the naive version fails:
//
//   - When the prefix is NOT written (plaintext studio, explicit
//     CookieDomain), the prefixed name is ignored ENTIRELY rather than
//     preferred. Preferring it makes a config rollback a session-confusion
//     bug: flipping CookieSecure or CookieDomain back leaves the browser
//     holding a __Host- cookie this build can neither overwrite nor delete,
//     and a fresh login would then read the PREVIOUS user's session.
//   - `acceptLegacy` is for the migration, and only the refresh cookie gets
//     it. Accepting a legacy ACCESS cookie reopens the very fixation it
//     closes: the prefixed access cookie expires in 15 minutes while a tossed
//     bare one is attacker-controlled and can outlive it, so every idle tab
//     past the access TTL would silently adopt the attacker's session — and
//     logout cannot clear a Domain-scoped cookie, so "log out, reload" would
//     land on their account. A legacy browser instead pays one 401, which the
//     SPA answers with a silent refresh (api/client.ts), and comes back fully
//     migrated.
func (s *Server) sessionCookie(r *http.Request, base string, acceptLegacy bool) string {
	if s.usesHostPrefix() {
		if c, err := r.Cookie(hostCookiePrefix + base); err == nil && c != nil && c.Value != "" {
			return c.Value
		}
		if !acceptLegacy {
			return ""
		}
	}
	if c, err := r.Cookie(base); err == nil && c != nil {
		return c.Value
	}
	return ""
}

func (s *Server) setAuthCookies(w http.ResponseWriter, access string, accessExp time.Time, refresh string, refreshExp time.Time) {
	access = strings.TrimSpace(access)
	if access != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     s.authCookieWriteName(authCookieName),
			Value:    access,
			Path:     "/",
			Domain:   s.cfg.CookieDomain,
			HttpOnly: true,
			Secure:   s.cfg.CookieSecure,
			SameSite: http.SameSiteLaxMode,
			Expires:  accessExp,
		})
	}
	if refresh != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     s.authCookieWriteName(refreshCookieName),
			Value:    refresh,
			Path:     s.refreshCookiePath(),
			Domain:   s.cfg.CookieDomain,
			HttpOnly: true,
			Secure:   s.cfg.CookieSecure,
			SameSite: http.SameSiteLaxMode,
			Expires:  refreshExp,
		})
		// RELEASE N ONLY — remove in N+1, together with the legacy READ in
		// sessionCookie.
		//
		// The refresh cookie has an OUT-OF-PROCESS consumer: the desktop app
		// harvests the rotated token from Set-Cookie, and it is a separately
		// installed binary (.deb / AppImage / macOS) that updates on its own
		// schedule, against a server that may already have flipped. A desktop
		// built before this branch matches only the bare name, so it harvests
		// "", keeps the previous token and replays it — which the server reads
		// as theft and answers by revoking EVERY session that user holds.
		//
		// Giving the read a two-release migration and the write none would
		// have left exactly that hole for anyone who updates the server first,
		// which is the normal order. So the legacy name is written alongside
		// for one release. It carries the same value, and reads prefer the
		// prefixed one, so a tossed bare cookie still cannot win.
		if s.usesHostPrefix() && os.Getenv("ITERION_LEGACY_REFRESH_COOKIE") != "0" {
			http.SetCookie(w, &http.Cookie{
				Name:     refreshCookieName,
				Value:    refresh,
				Path:     "/api/auth",
				Domain:   s.cfg.CookieDomain,
				HttpOnly: true,
				Secure:   s.cfg.CookieSecure,
				SameSite: http.SameSiteLaxMode,
				Expires:  refreshExp,
			})
		}
	}
}

// refreshCookiePath is /api/auth normally, but __Host- mandates Path=/.
// Trading the narrower path for the prefix is deliberate: path scoping is not
// a security boundary (the cookie is HttpOnly either way, and any same-origin
// page can cause a request to any path), whereas the prefix is what makes the
// cookie unforgeable by a sibling host.
func (s *Server) refreshCookiePath() string {
	if s.usesHostPrefix() {
		return "/"
	}
	return "/api/auth"
}

// agentBindingCookiePath returns the path for a per-flow CSRF-binding cookie:
// its natural narrow path, or "/" where the __Host- prefix applies (which
// mandates it). Same trade as refreshCookiePath — the path was never the
// boundary; being unwritable by a sibling host is.
func (s *Server) agentBindingCookiePath(narrow string) string {
	if s.usesHostPrefix() {
		return "/"
	}
	return narrow
}

// clearAuthCookies expires BOTH spellings. During the migration a browser can
// hold the legacy cookie and the prefixed one at once; clearing only the name
// this build writes would leave the other live and log the user back in.
func (s *Server) clearAuthCookies(w http.ResponseWriter) {
	type target struct{ name, path string }
	targets := []target{
		{authCookieName, "/"},
		{refreshCookieName, "/api/auth"},
		{hostCookiePrefix + authCookieName, "/"},
		{hostCookiePrefix + refreshCookieName, "/"},
	}
	for _, t := range targets {
		domain := s.cfg.CookieDomain
		secure := s.cfg.CookieSecure
		if strings.HasPrefix(t.name, hostCookiePrefix) {
			// A __Host- deletion is only accepted on the same terms as the
			// write that created it.
			domain = ""
			secure = true
		}
		http.SetCookie(w, &http.Cookie{
			Name:     t.name,
			Value:    "",
			Path:     t.path,
			Domain:   domain,
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})
	}
}

func (s *Server) refreshTokenFromRequest(r *http.Request) string {
	// acceptLegacy: a session minted before the migration must keep
	// working until it expires, and the refresh token is server-verified,
	// single-use and rotating — a far smaller window than the access cookie.
	if v := s.sessionCookie(r, refreshCookieName, true); v != "" {
		return v
	}
	// Fallback for SDK clients that send it in the body via header.
	if h := r.Header.Get("X-Iterion-Refresh"); h != "" {
		return h
	}
	return ""
}

// ---- Anonymous handlers ----

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" || req.Password == "" {
		httpError(w, http.StatusBadRequest, "email and password required")
		return
	}
	res, err := s.authSvc.Login(r.Context(), req.Email, req.Password, r.UserAgent(), s.clientIP(r))
	if err != nil {
		// Collapse "account disabled" and "invalid credentials" to the
		// same wire message so an attacker can't enumerate which
		// addresses correspond to disabled accounts. The detailed err
		// stays available in logs.
		if errors.Is(err, auth.ErrAccountDisabled) || errors.Is(err, auth.ErrInvalidCredentials) {
			s.markLogin("invalid")
			httpError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		// Password verified but pending_password_change status: surface
		// the explicit signal so the SPA routes to the change-password
		// flow. Don't mint cookies here — issuing tokens before the
		// password is rotated would defeat the gate entirely.
		if errors.Is(err, auth.ErrPasswordChangeRequired) {
			s.markLogin("password_change_required")
			httpError(w, http.StatusForbidden, "password change required")
			return
		}
		// Lockout deliberately surfaces as ErrInvalidCredentials above
		// (timing-indistinguishable), so there is no separate "locked"
		// label — anything else is an internal error.
		s.markLogin("error")
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.markLogin("success")
	s.renderAuthResponse(w, r, res)
}

// markLogin bumps the password-login outcome counter (no-op without a
// metrics registry).
func (s *Server) markLogin(result string) {
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.AuthLoginsTotal.WithLabelValues(result).Inc()
	}
}

// handleChangePassword completes the forced-rotation flow for a
// pending_password_change account: verify the temp password, set the new
// one, activate, and return a session. Errors map opaquely (401 for a bad
// email/temp/status, 400 for a too-weak new password) so the endpoint can't
// be used to probe account existence or state.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" || req.CurrentPassword == "" || req.NewPassword == "" {
		httpError(w, http.StatusBadRequest, "email, current_password and new_password required")
		return
	}
	res, err := s.authSvc.ChangePasswordPending(r.Context(), req.Email, req.CurrentPassword, req.NewPassword, r.UserAgent(), s.clientIP(r))
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.renderAuthResponse(w, r, res)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" || req.Password == "" {
		httpError(w, http.StatusBadRequest, "email and password required")
		return
	}
	res, err := s.authSvc.Register(r.Context(), req.Email, req.Password, req.Name, req.Invitation, r.UserAgent(), s.clientIP(r))
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.renderAuthResponse(w, r, res)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	tok := s.refreshTokenFromRequest(r)
	if tok == "" {
		httpError(w, http.StatusUnauthorized, "no refresh token")
		return
	}
	res, err := s.authSvc.Refresh(r.Context(), tok, r.UserAgent(), s.clientIP(r))
	if err != nil {
		s.clearAuthCookies(w)
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.renderAuthResponse(w, r, res)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	tok := s.refreshTokenFromRequest(r)
	if tok != "" {
		// Logout stays 204 regardless (the client's cookies are cleared
		// either way), but a failed revocation leaves the refresh token
		// live server-side — make it visible.
		if err := s.authSvc.Logout(r.Context(), tok); err != nil && s.logger != nil {
			s.logger.Error("auth: revoke refresh token on logout: %v", err)
		}
	}
	s.clearAuthCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

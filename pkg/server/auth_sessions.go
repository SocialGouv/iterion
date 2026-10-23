package server

import (
	"errors"
	"net/http"
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
//   - The legacy dual-name read (the migration's `acceptLegacy` parameter,
//     refresh cookie only) is gone: accepting a legacy ACCESS cookie would
//     reopen the very fixation it closed — the prefixed access cookie expires
//     in 15 minutes while a tossed bare one is attacker-controlled and can
//     outlive it, so every idle tab past the access TTL would silently adopt
//     the attacker's session, and logout cannot clear a Domain-scoped cookie.
//     A legacy browser instead pays one 401, which the SPA answers with a
//     silent refresh (api/client.ts), and comes back fully migrated.
func (s *Server) sessionCookie(r *http.Request, base string) string {
	if s.usesHostPrefix() {
		if c, err := r.Cookie(hostCookiePrefix + base); err == nil && c != nil && c.Value != "" {
			return c.Value
		}
		// A host-prefixed deployment refuses the bare name outright: the
		// migration's dual-name read is gone (removed 2026-10, #1076).
		return ""
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
			Expires:  refreshCookieExpiry(refreshExp),
		})
	}
}

// refreshCookieExpiry is the Expires a refresh cookie carries: the session's
// expiry rounded UP to the hour. A refresh session's expiry is its rotation's
// instant plus the TTL, and a token presented again inside the reuse window
// is answered with the session that rotation wrote — so an exact expiry would
// date, to the second, the rotation an HTTP client holding the token was
// answered from. Rounded to the hour it hides WHEN that rotation ran, not
// THAT the answer came from an earlier rotation: an hour boundary falling
// between the rotation and the redelivery shows it, and so does the answer's
// latency — a rotation writes where a redelivery only reads. Rounded up, the
// cookie never dies before the session it carries.
func refreshCookieExpiry(exp time.Time) time.Time {
	if exp.IsZero() {
		return exp
	}
	up := exp.Truncate(time.Hour)
	if up.Before(exp) {
		up = up.Add(time.Hour)
	}
	return up
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
	// The legacy bare name is no longer read: both halves of the migration
	// are gone (docs/browser-security.md records the two conditions that
	// gated the removal). The refresh token is server-verified, single-use
	// and rotating, so the prefixed name is the only spelling.
	if v := s.sessionCookie(r, refreshCookieName); v != "" {
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
		s.authError(w, r, err)
		return
	}
	s.markLogin("success")
	s.writeSession(w, r, loginSession(res))
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
		s.credentialChangeError(w, r, err)
		return
	}
	s.writeSession(w, r, loginSession(res))
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
		s.authError(w, r, err)
		return
	}
	s.writeSession(w, r, loginSession(res))
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	tok := s.refreshTokenFromRequest(r)
	if tok == "" {
		httpError(w, http.StatusUnauthorized, "no refresh token")
		return
	}
	res, err := s.authSvc.Refresh(r.Context(), tok, r.UserAgent(), s.clientIP(r))
	if err != nil {
		// Only a refusal of the credential itself (401: revoked, expired,
		// unknown) clears the cookies carrying it. A failure leaves the
		// presented token the client's to present again — Refresh rotates
		// nothing before every read it needs has succeeded.
		if mapAuthErrorStatus(err) == http.StatusUnauthorized {
			s.clearAuthCookies(w)
		}
		s.authError(w, r, err)
		return
	}
	s.writeSession(w, r, loginSession(res))
}

// LogoutOutcomeHeader, set to LogoutOtherSession, marks a 204 of POST
// /api/auth/logout?lineage=<id> that signed nothing out: the presented token
// belongs to another lineage — a session the client opened since the one it
// meant to end — and its cookies are kept. Every other 204 means the
// presented session is signed out, or there was none.
const (
	LogoutOutcomeHeader = "X-Iterion-Logout"
	LogoutOtherSession  = "other-session"
)

// handleLogout signs out the session of the presented refresh token (its
// lineage) and clears the auth cookies. With ?lineage=<id>, a token of
// another lineage signs nothing out: the answer is 204 with
// X-Iterion-Logout: other-session and the cookies are kept — a client whose
// pending sign-out names a session it no longer holds never ends the one it
// holds now. A sign-out whose read or record failed is not done: it answers
// the failure (identityFailure) and leaves the cookies, so the client still
// holds the token to present again — clearing them would drop the only
// credential that can still end a live session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok := s.refreshTokenFromRequest(r); tok != "" {
		signedOut, err := s.authSvc.LogoutOf(r.Context(), tok, r.URL.Query().Get("lineage"))
		if err != nil {
			s.identityFailure(w, r, err)
			return
		}
		if !signedOut {
			w.Header().Set(LogoutOutcomeHeader, LogoutOtherSession)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	s.clearAuthCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

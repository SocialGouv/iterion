package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
)

func (s *Server) setAuthCookies(w http.ResponseWriter, access string, accessExp time.Time, refresh string, refreshExp time.Time) {
	access = strings.TrimSpace(access)
	if access != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     authCookieName,
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

func (s *Server) clearAuthCookies(w http.ResponseWriter) {
	for _, name := range []string{authCookieName, refreshCookieName} {
		path := "/"
		if name == refreshCookieName {
			path = "/api/auth"
		}
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     path,
			Domain:   s.cfg.CookieDomain,
			HttpOnly: true,
			Secure:   s.cfg.CookieSecure,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})
	}
}

func (s *Server) refreshTokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie(refreshCookieName); err == nil && c != nil {
		return c.Value
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

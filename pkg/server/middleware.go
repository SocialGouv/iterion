package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// authCookieName is the HttpOnly cookie carrying the access JWT for
// the SPA. CLI / SDK clients use Authorization: Bearer <jwt>.
const authCookieName = "iterion_auth"

// refreshCookieName is the HttpOnly cookie carrying the refresh
// token. Scoped to the /api/auth path so it never leaves the auth
// endpoints.
const refreshCookieName = "iterion_refresh"

// requireAuth wraps next with JWT verification. On success it
// injects the resolved Identity into the request context.
//
// Health endpoints (/healthz, /readyz) and unauthenticated /auth/*
// routes (login/register/refresh/logout) bypass this middleware via
// the public-route check in (*Server).withAuth — this function only
// runs once that gate has matched.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.DisableAuth {
			// Dev mode: synthesize a super-admin identity so handlers
			// behave as if the request was authenticated. Never use
			// in production.
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{
				UserID:       "dev",
				Email:        "dev@local",
				IsSuperAdmin: true,
			})))
			return
		}
		token := extractBearer(r)
		if token == "" {
			httpError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if s.signer == nil {
			httpError(w, http.StatusInternalServerError, "auth not configured")
			return
		}
		id, err := s.signer.Verify(token)
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrTokenExpired):
				httpError(w, http.StatusUnauthorized, "token expired")
			default:
				httpError(w, http.StatusUnauthorized, "token invalid")
			}
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
	})
}

// requireRole wraps next, ensuring the principal has at least the
// given role *in their active team*. Super-admins always pass.
func (s *Server) requireRole(want identity.Role, next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		if !id.HasRole(want) {
			httpError(w, http.StatusForbidden, "insufficient permissions")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// requireSuperAdmin wraps next, allowing only platform super-admins.
func (s *Server) requireSuperAdmin(next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		if !id.IsSuperAdmin {
			httpError(w, http.StatusForbidden, "super-admin only")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// extractBearer pulls the access JWT from the Authorization header
// or the auth cookie, returning the empty string if neither is set.
func extractBearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if c, err := r.Cookie(authCookieName); err == nil && c != nil {
		return c.Value
	}
	// Browsers can't attach Authorization headers to a WS upgrade,
	// so we accept ?t=<jwt> on /api/ws/* (same-origin only).
	if t := r.URL.Query().Get("t"); t != "" && strings.HasPrefix(r.URL.Path, "/api/ws/") {
		return t
	}
	return ""
}

// isPublicPath reports whether the path is reachable without auth.
// Health probes + the auth/oidc bootstrap routes live here.
func isPublicPath(path string) bool {
	switch {
	case path == "/healthz", path == "/readyz":
		return true
	case strings.HasPrefix(path, "/api/auth/login"),
		strings.HasPrefix(path, "/api/auth/register"),
		strings.HasPrefix(path, "/api/auth/refresh"),
		strings.HasPrefix(path, "/api/auth/logout"),
		strings.HasPrefix(path, "/api/auth/oidc/"),
		strings.HasPrefix(path, "/api/auth/invitations/lookup"),
		strings.HasPrefix(path, "/api/auth/invitations/accept"),
		strings.HasPrefix(path, "/api/auth/providers"):
		return true
	}
	// Static files / SPA bundle.
	if path == "/" || strings.HasPrefix(path, "/assets/") || strings.HasPrefix(path, "/static/") {
		return true
	}
	if !strings.HasPrefix(path, "/api/") {
		return true
	}
	return false
}

// authMiddleware is the umbrella middleware applied to every
// request. It bypasses auth for public paths, otherwise enforces
// requireAuth (which puts the Identity in ctx). Per-handler role
// gates (requireRole / requireSuperAdmin) re-wrap inside.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		s.requireAuth(next).ServeHTTP(w, r)
	})
}

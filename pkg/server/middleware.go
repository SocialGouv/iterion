package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/pat"
	"github.com/SocialGouv/iterion/pkg/store"
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
			// in production. Stamp a stable "dev" tenant_id/user_id
			// onto the store-level ctx so the mongo store's fail-
			// closed tenant guard accepts writes (otherwise SaveRun
			// panics on cross-tenant queries). Filesystem store
			// ignores the tag; mongo scopes the dev-mode data under
			// one synthetic tenant — fine for local + cloud-e2e use.
			ctx := auth.WithIdentity(r.Context(), auth.Identity{
				UserID:       "dev",
				Email:        "dev@local",
				IsSuperAdmin: true,
			})
			ctx = store.WithIdentity(ctx, "dev", "dev")
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		// Single-use WS ticket path (?ticket= on /api/ws[/*]): the ticket
		// resolves DIRECTLY to an identity, so it runs before the JWT/PAT
		// verification. ok=true with an empty UserID means a ticket was
		// presented but was invalid/expired/used — reject rather than falling
		// through to the (absent) bearer.
		if id, ok := s.wsTicketIdentity(r); ok {
			if id.UserID == "" {
				httpError(w, http.StatusUnauthorized, "invalid or expired ws ticket")
				return
			}
			ctx, ok := s.stampAuthedContext(w, r, id)
			if !ok {
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		token := extractBearer(r)
		if token == "" {
			httpError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		var id auth.Identity
		if strings.HasPrefix(token, pat.TokenPrefix) {
			// Personal access token path (programmatic clients). The
			// prefix branch keeps the JWT hot path allocation-free.
			// Fail-closed: an iap_ bearer with no PAT store is a 401,
			// never a fall-through to JWT parsing.
			var err error
			id, err = s.identityFromPAT(r.Context(), token)
			if err != nil {
				httpError(w, http.StatusUnauthorized, "%s", err.Error())
				return
			}
		} else {
			if s.signer == nil {
				httpError(w, http.StatusInternalServerError, "auth not configured")
				return
			}
			var err error
			id, err = s.signer.Verify(token)
			if err != nil {
				switch {
				case errors.Is(err, auth.ErrTokenExpired):
					httpError(w, http.StatusUnauthorized, "token expired")
				default:
					httpError(w, http.StatusUnauthorized, "token invalid")
				}
				return
			}
		}
		// Stamp both the auth identity (for handlers + RBAC checks)
		// and the store-level tenant_id / user_id (for the Mongo
		// query filters in pkg/store/mongo). The store layer keeps
		// its own keys so it can stay independent of pkg/auth.
		ctx, ok := s.stampAuthedContext(w, r, id)
		if !ok {
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// tenantFreePrefixes lists the routes an authenticated but TEAM-LESS
// identity may still reach: the account/tenancy surfaces needed to GET
// an active team (profile, sessions, switch-team — which mints a
// teamful JWT — under /api/auth/), org bootstrap (/api/orgs), and the
// super-admin surfaces (/api/admin/), which query the store under
// WithoutTenantFilter by design (non-admins are 403'd by
// requireSuperAdmin regardless). Everything else is tenant-scoped:
// letting a team-less ctx through used to reach the Mongo store's
// fail-closed tenant guard as a recovered PANIC on every request — a
// silent 500 for the caller and pure noise burying real crashes
// (Sentry ITERION-13/-1W/-1Z, 1800+ events). The rejection below is
// explicit and actionable instead; the store guard stays as the
// last-resort backstop.
//   - /api/auth/  : profile, sessions, switch-team (mints a teamful JWT)
//   - /api/me     : USER-scoped resources (oauth connections, sso links —
//     their handlers key on id.UserID, never the tenant)
//   - /api/orgs   : org bootstrap/listing
//   - /api/teams/ : team administration scoped by the PATH's team id —
//     the handlers resolve membership and stamp the target
//     team themselves; the caller's ACTIVE team is
//     irrelevant there by design
//   - /api/admin/ : super-admin surfaces (WithoutTenantFilter by design;
//     non-admins are 403'd by requireSuperAdmin)
//
// Prefixes end with "/" and are matched segment-exactly below —
// "/api/me/" must NOT admit "/api/memory/..." (tenant-scoped workspace
// memory), which a bare HasPrefix("/api/me") would.
var tenantFreePrefixes = []string{"/api/auth/", "/api/me/", "/api/orgs/", "/api/teams/", "/api/admin/"}

func tenantFreePath(p string) bool {
	return tenantFreePathMethod("", p)
}

// tenantFreePathMethod is tenantFreePath plus the method-scoped public
// surfaces: marketplace READS are public for anonymous callers, so a
// signed-in but team-less viewer must not get LESS than an anonymous
// one (gate F2 — the choke used to 403 before the routing bypass could
// apply). The handlers are tenant-agnostic for a caller with no tenant.
func tenantFreePathMethod(method, p string) bool {
	if isPublicMarketplaceRead(method, p) {
		return true
	}
	for _, pre := range tenantFreePrefixes {
		if strings.HasPrefix(p, pre) || p == strings.TrimSuffix(pre, "/") {
			return true
		}
	}
	return false
}

// stampAuthedContext writes the resolved identity onto the request
// context — the auth identity always, the store-level tenant only when
// the identity carries one. A team-less identity on a tenant-scoped
// route is refused here (the ONE choke point for every requireAuth
// route), never handed to the store with an empty tenant.
func (s *Server) stampAuthedContext(w http.ResponseWriter, r *http.Request, id auth.Identity) (context.Context, bool) {
	ctx := auth.WithIdentity(r.Context(), id)
	if id.TeamID == "" {
		if !tenantFreePathMethod(r.Method, r.URL.Path) {
			httpError(w, http.StatusForbidden,
				"credential has no active team — switch to a team in the studio, or re-create the token/ticket with an explicit team, before calling tenant-scoped endpoints")
			return nil, false
		}
		// No tenant to stamp: tenant-free handlers read the auth
		// identity (or opt out via WithoutTenantFilter); any
		// tenant-scoped store call remains guarded fail-closed.
		return ctx, true
	}
	return store.WithIdentity(ctx, id.TeamID, id.UserID), true
}

// isSuperAdmin reports whether the request carries a platform
// super-admin identity. Local DisableAuth mode always passes — that is
// the same outcome requireAuth produces there (it synthesizes a
// super-admin identity for every request), stated directly so the
// predicate holds even when a caller sits outside the auth middleware.
func (s *Server) isSuperAdmin(r *http.Request) bool {
	if s.cfg.DisableAuth {
		return true
	}
	id, _ := auth.FromContext(r.Context())
	return id.IsSuperAdmin
}

// requireSuperAdmin wraps next, allowing only platform super-admins.
func (s *Server) requireSuperAdmin(next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.isSuperAdmin(r) {
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
	// so we accept ?t=<jwt> on the WS endpoints (same-origin only).
	// We match both /api/ws (the file-event hub at exactly that path)
	// and /api/ws/* (per-run streams under /api/ws/runs/<id>).
	//
	// SECURITY / future work: this branch is dead for every shipped
	// client today — the browser/cloud SPA authenticates the same-origin
	// WS via the HttpOnly cookie (it never appends ?t=), and the desktop
	// build runs the embedded server with DisableAuth=true and returns an
	// empty session token, so nothing puts a JWT in the URL (see
	// cmd/iterion-desktop/bindings.go GetSessionToken). It is kept only
	// for a future hosted-desktop-with-auth build. When that lands, do
	// NOT carry the long-lived access JWT in the URL — query strings leak
	// to access logs, proxies and Referer. Replace this with a single-use,
	// short-TTL ticket: an authenticated POST /api/ws/ticket mints an
	// opaque ticket bound to the identity, the client passes it as
	// ?ticket=, and the server consumes (and invalidates) it before the
	// upgrade.
	if t := r.URL.Query().Get("t"); t != "" && (r.URL.Path == "/api/ws" || strings.HasPrefix(r.URL.Path, "/api/ws/")) {
		return t
	}
	return ""
}

// isPublicPath reports whether the path is reachable without auth.
// Health probes + the auth/oidc bootstrap routes live here.
//
// Leaf endpoints use exact match so e.g. `/api/auth/loginXYZ` cannot
// sneak in. Namespace prefixes (`/api/auth/oidc/`, `/assets/`) keep
// HasPrefix because they intentionally cover every sub-path.
func isPublicPath(path string) bool {
	switch path {
	case "/healthz", "/readyz",
		"/api/auth/login",
		"/api/auth/password/change",
		"/api/auth/password/reset/request",
		"/api/auth/password/reset/confirm",
		"/api/auth/register",
		"/api/auth/refresh",
		"/api/auth/logout",
		"/api/auth/providers",
		"/api/auth/desktop/exchange",
		"/api/auth/invitations/lookup",
		"/api/auth/invitations/accept",
		// /api/server/info carries the AuthRequired flag the SPA
		// reads before deciding whether to show Login. It must
		// always be reachable.
		"/api/server/info",
		"/", "/index.html":
		return true
	}
	if strings.HasPrefix(path, "/api/auth/oidc/") {
		return true
	}
	// The forge OAuth callback is a top-level GET navigation from the forge
	// IdP carrying no operator JWT — it authenticates via the signed state +
	// the per-flow agent-binding cookie, like the OIDC callback above.
	if path == "/api/forge/oauth/callback" || path == "/api/forge/github/app/callback" ||
		path == "/api/forge/github/app-manifest/callback" {
		return true
	}
	// Inbound webhooks authenticate themselves via a per-org token
	// (webhookAuth), not the operator JWT — bypass the JWT gate so the
	// middleware never rejects a tokened forge call as "unauthenticated".
	if strings.HasPrefix(path, "/api/webhooks/") {
		return true
	}
	// Config-share editor: self-authenticated by configShareAuth (a per-share
	// Bearer iws_ token), not the operator JWT — bypass the JWT gate.
	if strings.HasPrefix(path, "/api/config-share/") {
		return true
	}
	// Per-run X-Iterion-Run token surfaces (board MCP HTTP transport,
	// deterministic forge review publishing): the handler authenticates
	// the run token itself and 401s on a missing/unknown one, so the
	// operator-JWT gate must not front them — a sandboxed or
	// runner-launched run carries no JWT.
	if strings.HasPrefix(path, "/api/v1/mcp/") || path == "/api/v1/forge/publish-review" {
		return true
	}
	if strings.HasPrefix(path, "/assets/") || strings.HasPrefix(path, "/static/") {
		return true
	}
	if !strings.HasPrefix(path, "/api/") {
		return true
	}
	return false
}

// isPublicMarketplaceRead reports whether (method, path) is a marketplace
// endpoint readable WITHOUT authentication: browse, detail, config, and
// .botz download — all GET. The mutating + privileged endpoints
// (POST /submit, POST|DELETE …/install, GET …/moderation) stay behind
// auth. Detail (`…/bots/{slug}`) and download (`…/bots/{slug}/download`)
// share the `/bots/` prefix; install is GET-excluded already but we keep
// the suffix check explicit so the intent survives future edits.
//
// Kept separate from isPublicPath because that one is method-agnostic:
// folding a method-aware rule into it would risk opening a POST by the
// same path. The caller (authMiddleware) only bypasses auth for an
// anonymous caller, so a signed-in viewer still gets org-scoped
// visibility via requireAuth.
func isPublicMarketplaceRead(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	switch path {
	case "/api/v1/marketplace/bots", "/api/v1/marketplace/config":
		return true
	}
	if strings.HasPrefix(path, "/api/v1/marketplace/bots/") {
		return !strings.HasSuffix(path, "/install")
	}
	return false
}

// authMiddleware is the umbrella middleware applied to every
// request. It bypasses auth for public paths, otherwise dispatches
// to a pre-built authenticated handler so the per-request hot path
// allocates no closures.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	authed := s.requireAuth(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// Marketplace reads are public for anonymous callers (landing-page
		// browse / .botz download). A request that carries a credential
		// still goes through requireAuth so a signed-in viewer keeps
		// org-scoped visibility; only the no-credential case bypasses.
		// Dev mode (DisableAuth) keeps going through requireAuth so its
		// synthesized super-admin identity is injected.
		if !s.cfg.DisableAuth && extractBearer(r) == "" &&
			isPublicMarketplaceRead(r.Method, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		authed.ServeHTTP(w, r)
	})
}

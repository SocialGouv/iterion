package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/store"
)

// tenantScopeHeader is the wire name of the cross-team scope header a caller
// sends to override the JWT's active team on a route that scopes by tenant —
// the counterpart of the `?team_id=` query. The CLI's
// `iterion remote runs stats|repos --team <id>` sends the query form; the
// header exists for callers whose base URL is assembled for them
// (ITERION_REMOTE_TEAM) and for future surfaces. The name is stable and
// greppable so a middleware sweep, an audit log or a `curl -H` reproduce
// every scoped call.
const tenantScopeHeader = "X-Iterion-Team"

// resolveTenantScope resolves the tenant a caller wants to read from an
// `/api/v1/*` route that scopes by tenant. It reads (in precedence order)
// the `?team_id=` query, the tenantScopeHeader, then falls back to the
// caller's active team from the JWT.
//
// A non-active team requires authorisation — a super-admin sees every
// tenant; every other user only the teams they belong to via canViewTeam
// (which honours direct membership and the org-admin lineage). An
// unauthorised scope is a typed 403 naming the parameter: never
// accepted-and-dropped (#1419 was exactly that: the endpoint took the
// parameter and silently ignored it, so an operator comparing three
// tenants got byte-identical answers).
//
// Returns the resolved teamID, a ctx re-scoped to it (via store.WithTenant),
// and ok=true when the caller may read that tenant. On refusal ok=false is
// returned AND the response has been written; the caller must return
// immediately.
//
// This helper is the single choke-point for the scoping-parameter class:
// every /api/v1/* handler that reads runs/stats/insights of a tenant goes
// through it (bus_conformance-style: the class holds by construction).
// Sites that already scope through /api/teams/{id}/... in the path do NOT
// use this helper — path scoping is explicit and already covered by the
// existing canViewTeam guard.
func (s *Server) resolveTenantScope(w http.ResponseWriter, r *http.Request) (string, context.Context, bool) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		httpError(w, http.StatusUnauthorized, "authentication required")
		return "", nil, false
	}
	// The requested team: query wins over header (query is closer to the
	// call site the operator wrote), header falls back for CLIs whose base
	// URL is opaque to their user (ITERION_REMOTE_TEAM).
	requested := strings.TrimSpace(r.URL.Query().Get("team_id"))
	if requested == "" {
		requested = strings.TrimSpace(r.Header.Get(tenantScopeHeader))
	}
	// No override → use the JWT's active team. This is the default and
	// keeps every pre-existing call site working unchanged. Always
	// re-stamp the tenant on the returned ctx from the RESOLVED id: the
	// middleware already put it there for authenticated requests, but a
	// synthetic caller (webhook/share) may not have — the helper is a
	// choke point, so it certifies the ctx it hands back.
	if requested == "" || requested == id.TeamID {
		return id.TeamID, store.WithTenant(r.Context(), id.TeamID), true
	}
	// An override must clear authorisation. canViewTeam is the exact
	// property we want: super-admin cross-tenant, direct membership, org
	// admin/owner of the target team's parent org. Anything else is a 403
	// that NAMES the parameter — never a silent drop.
	if !s.canViewTeam(r.Context(), id, requested) {
		httpError(w, http.StatusForbidden, "not authorised to scope this route to team_id=%s", requested)
		return "", nil, false
	}
	return requested, store.WithTenant(r.Context(), requested), true
}

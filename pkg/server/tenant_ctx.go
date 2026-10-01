package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
)

// teamPathTenantCtx scopes the store context to the team a route names in
// its PATH, instead of the caller's ACTIVE team that requireAuth stamped.
//
// WHY THIS EXISTS, and why every tenant-scoped handler under
// /api/teams/{id}/... must use it:
//
// The auth middleware stamps ONE tenant on the request — the one on the
// caller's JWT (stampAuthedContext). The tenant-scoped stores derive
// everything from that context: on write they STAMP the row's tenant_id,
// on read they FILTER on it. Meanwhile authorization is checked against
// the team in the PATH (canManageTeam admits a super-admin, and an org
// admin over any team of their org). So the two routinely differ — that
// divergence IS the documented way an SRE wires another team's
// credential.
//
// When they differ and the handler forgets to re-scope, the row lands as
// (scope_team = the path team, tenant_id = the caller's active team):
// invisible from the target team's list, invisible to that team's runs,
// and the API answers 201. Nothing fails — the bot simply runs without
// the credential it was given, which is the one shape a credential bug
// must never take.
//
// Paid twice: once on the BYOK api-keys (fixed at that site only), then
// again on generic secrets AND bot bindings, measured in production on
// 2026-09-08 while wiring a Jira credential — a review reported
// "unverifiable: tracker token file does not exist" for a secret the
// console showed as created (#997, docs/bot-runs/review-pr.md).
//
// Routes with no {id} (the /api/me family) keep the active team: there
// the caller's own tenant IS the scope.
func teamPathTenantCtx(r *http.Request) context.Context {
	return teamTenantCtx(r.Context(), r.PathValue("id"))
}

// teamTenantCtx is the explicit-team form, for handlers that already hold
// the team id (or read it from another path segment).
func teamTenantCtx(ctx context.Context, teamID string) context.Context {
	if teamID == "" {
		return ctx
	}
	return store.WithTenant(ctx, teamID)
}

// canonicalizeTeamPathValue rewrites the {id} path value of a
// /api/teams/{id}/… route to the team's UUID when the caller spelled its
// SLUG — the ONE place the two spellings are reconciled (#1931). The
// routes accept both spellings (canManageTeam passes a super-admin either
// way, and the CLI passes --team verbatim), but every team-scoped row was
// keyed by the RAW path value — the binding's tenant_id, the secret's
// scope_team, the ctx tenant the stores stamp on write and filter on read
// — so a row written via the slug was invisible to every reader resolving
// under the UUID: the publisher at launch found 0 binding and ran the bot
// WITHOUT its credential, and a cross-spelling PATCH failed the misleading
// "not a team-scoped secret in this org" 400. Canonical here, at route
// resolution, and every handler downstream — teamPathTenantCtx's stamp,
// the ScopeTeamID == teamID comparisons, the tenant_id filters — is
// spelling-insensitive by construction, with no per-reader guard.
//
// A spelling that IS a team id wins over a slug of the same text (the id
// is the authority); an unresolvable spelling is left as is, so an unknown
// team keeps its old 403/404 semantics. Non-team routes keep their {id} —
// a run id that happens to equal a team slug is nobody's team.
//
// The return reports whether the route's {id} names a team at all: false
// ONLY for a team route whose spelling resolves to NOTHING (neither an id
// nor a slug) — the ghost a MUTATING route must refuse (#2046, see
// requireAuth) rather than let a super-admin key rows by, invisible to
// every UUID reader. true for non-team routes and a nil auth store, which
// have no ghost to refuse.
//
// An OUTAGE is not an absence: only a clean ErrNotFound from BOTH lookups
// makes the spelling a ghost (the backends map not-found alone onto the
// sentinel — any other error is a store failure). Refusing on a failed
// read would 404 a REAL team "unknown team" mid-incident and misdirect
// the response; deferring lets the handler surface the outage instead.
func (s *Server) canonicalizeTeamPathValue(r *http.Request) bool {
	raw := r.PathValue("id")
	if raw == "" || !teamIDRoutePattern(r.Pattern) {
		return true
	}
	st := s.authStore()
	if st == nil {
		return true
	}
	_, err := st.GetTeam(r.Context(), raw)
	if err == nil {
		return true
	}
	// The slug lookup runs on ANY GetTeam error, as #2043 always did: a
	// Mongo failover (id read timed out, retry lands on the new primary)
	// still canonicalizes, instead of leaving the raw slug to key a
	// mutating call's rows — the #1931 split the ghost refusal could not
	// name. The error gate below governs the ghost verdict, never this
	// fallthrough.
	t, serr := st.GetTeamBySlug(r.Context(), raw)
	if serr == nil {
		r.SetPathValue("id", t.ID)
		return true
	}
	// A ghost is a clean not-found from BOTH lookups; an OUTAGE on either
	// side is not an absence (the backends map not-found alone onto the
	// sentinel), so it defers to the handler, which surfaces the store
	// failure — refusing here would 404 a REAL team "unknown team"
	// mid-incident and misdirect the response.
	return !(errors.Is(err, identity.ErrNotFound) && errors.Is(serr, identity.ErrNotFound))
}

// teamIDRoutePattern reports whether a registered route pattern is a team
// route — /api/teams/{id} itself or anything under it: the patterns whose
// {id} wildcard names a team. /api/orgs/{id}, /api/runs/{id} and every
// other id wildcard stay out of the class (patternWildcardAt's note on the
// run-id side is the same rule, seen from the other route family).
func teamIDRoutePattern(pattern string) bool {
	if i := strings.Index(pattern, " "); i >= 0 && !strings.HasPrefix(pattern, "/") {
		pattern = pattern[i+1:]
	}
	return pattern == "/api/teams/{id}" || strings.HasPrefix(pattern, "/api/teams/{id}/")
}

package server

import (
	"context"
	"net/http"

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

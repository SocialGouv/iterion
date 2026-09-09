package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Team lifecycle: rename, suspend, delete, and place an existing member.
//
// Teams were create-and-list only. Everything else about a team was
// immutable through the API even though the fields existed: Team.Status
// (suspended / read_only) is READ by the launch gate but nothing could
// write it, a naming mistake was permanent, and an empty team created by
// accident stayed forever. And an org admin could not put a user who
// already has an account into a team — the only path was an email
// invitation the user had to accept, which for a reorg where every account
// already exists is a round trip per person for no security gain (the
// admin already holds the right to set that member's role afterwards).
func (s *Server) registerTeamLifecycleRoutes() {
	s.mux.Handle("GET /api/teams/{id}", s.requireAuth(http.HandlerFunc(s.handleGetTeam)))
	s.mux.Handle("PATCH /api/teams/{id}", s.requireAuth(http.HandlerFunc(s.handleUpdateTeam)))
	s.mux.Handle("POST /api/teams/{id}/status", s.requireAuth(http.HandlerFunc(s.handleSetTeamStatus)))
	s.mux.Handle("DELETE /api/teams/{id}", s.requireAuth(http.HandlerFunc(s.handleDeleteTeam)))
	s.mux.Handle("PUT /api/teams/{id}/members/{user_id}", s.requireAuth(http.HandlerFunc(s.handlePutTeamMember)))
	s.mux.Handle("PUT /api/orgs/{id}/members/{user_id}", s.requireAuth(http.HandlerFunc(s.handlePutOrgMember)))
}

// handlePutOrgMember places an EXISTING account in the org with an org
// role, idempotently — the org-level twin of handlePutTeamMember, and the
// half without which that one cannot serve the case it was written for: a
// user with no org at all could still only be reached by email, so the
// round trip was merely moved one level up.
//
// PATCH on the same path updates an EXISTING membership and 404s otherwise;
// this creates or updates. Org admin, like every other org roster write.
func (s *Server) handlePutOrgMember(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	memberID := r.PathValue("user_id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	role := identity.OrgRole(req.Role)
	if !role.Valid() {
		httpError(w, http.StatusBadRequest, "invalid org role (member|admin|owner)")
		return
	}
	o, err := s.authStore().GetOrg(r.Context(), orgID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	if o.Personal {
		httpError(w, http.StatusUnprocessableEntity, "a personal org takes no other member")
		return
	}
	u, err := s.authStore().GetUser(r.Context(), memberID)
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			httpError(w, http.StatusNotFound,
				"no such user — this endpoint places an EXISTING account; invite an unknown email with POST /api/orgs/{id}/invitations")
			return
		}
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	if u.Status == identity.UserStatusDisabled {
		httpError(w, http.StatusUnprocessableEntity, "user %s is disabled — re-enable the account before granting it an org", u.Email)
		return
	}
	if err := s.authStore().UpsertOrgMembership(r.Context(), identity.OrgMembership{
		UserID: memberID, OrgID: orgID, Role: role, JoinedAt: time.Now().UTC(),
	}); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditOrg(r, orgID, "org_member.added", "member", memberID, map[string]any{"role": string(role), "email": u.Email})
	writeJSON(w, map[string]any{"user_id": memberID, "org_id": orgID, "role": string(role)})
}

func (s *Server) handleGetTeam(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	t, err := s.authStore().GetTeam(r.Context(), teamID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	writeJSON(w, toTeamSummaryView(t))
}

func (s *Server) handleUpdateTeam(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req struct {
		Name *string `json:"name,omitempty"`
		Slug *string `json:"slug,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil && *req.Name == "" {
		httpError(w, http.StatusBadRequest, "name cannot be empty")
		return
	}
	if req.Slug != nil && *req.Slug == "" {
		httpError(w, http.StatusBadRequest, "slug cannot be empty")
		return
	}
	// PATCH, not read-modify-write: UpdateTeam replaces the whole document,
	// so a rename would carry back the Status it read and silently RESUME a
	// team another admin suspended in between — a governance action undone
	// by an unrelated edit.
	t, err := s.authStore().PatchTeam(r.Context(), teamID, identity.TeamPatch{Name: req.Name, Slug: req.Slug})
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "team.updated", "team", teamID, map[string]any{"name": t.Name, "slug": t.Slug})
	writeJSON(w, toTeamSummaryView(t))
}

// handleSetTeamStatus writes Team.Status, which the launch gate has always
// read and nothing could set. A suspended team keeps its data and its
// members; it launches no run.
func (s *Server) handleSetTeamStatus(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	// Suspension is an ORG-level decision: a team admin suspending their own
	// team is harmless, but a team admin RESUMING one their org suspended
	// would undo a governance action from inside the thing being governed.
	if !s.orgAdminOfTeam(r.Context(), id, teamID) && !id.IsSuperAdmin {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	var req struct {
		Status string `json:"status"`
		Reason string `json:"reason,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	st := identity.TeamStatus(req.Status)
	// pending_deletion is the ORG soft-delete state; a team has no purge
	// sweeper of its own, so accepting it here would park a team in a state
	// nothing ever resolves.
	if !identity.ValidTeamStatus(st) || st == identity.TeamStatusPendingDeletion {
		httpError(w, http.StatusBadRequest, "invalid status (active|suspended|read_only)")
		return
	}
	// The patch carries the suspension trio with the status — one fact, one
	// write — so a concurrent rename cannot revert it, and a resumed team
	// cannot keep a SuspendedAt.
	t, err := s.authStore().PatchTeam(r.Context(), teamID, identity.TeamPatch{
		Status: &st, SuspendedBy: id.UserID, SuspendReason: req.Reason,
	})
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "team.status_changed", "team", teamID, map[string]any{"status": string(st), "reason": req.Reason})
	if t.OrgID != "" {
		s.auditOrg(r, t.OrgID, "team.status_changed", "team", teamID, map[string]any{"status": string(st), "reason": req.Reason})
	}
	writeJSON(w, toTeamSummaryView(t))
}

// teamResidue names what still lives under a team, in the order an operator
// must tear it down. Empty means the team can be deleted with nothing left
// behind.
//
// This is a REFUSAL list, not a cascade. A team's data is spread across
// every tenant-scoped collection, and the only correct cascade is the one
// the org purge sweeper runs — which exists, is tested, and is bounded by a
// grace window. Deleting a team by hand here would have to reimplement it,
// and the one failure mode that matters is silent: a forge integration
// whose webhook keeps firing into a tenant that no longer exists.
//
// It covers what still FIRES or still holds a CREDENTIAL, which is the set
// whose survival is dangerous rather than merely untidy. What it does NOT
// cover, and deliberately: inert history (finished runs, board cards). The
// store exposes an ACTIVE-run count and no total, so blocking on history
// would mean inventing a seam for a case the org purge already serves —
// delete the org, not the team, when a team has a past worth removing.
func (s *Server) teamResidue(ctx context.Context, teamID string) []string {
	tctx := store.WithTenant(ctx, teamID)
	var out []string
	if s.forgeIntegrations != nil {
		if items, err := s.forgeIntegrations.ListByTenant(tctx, teamID); err != nil {
			out = append(out, fmt.Sprintf("repo integrations (unreadable: %v)", err))
		} else if len(items) > 0 {
			out = append(out, fmt.Sprintf("%d repo integration(s) — remove them first, or their webhooks keep firing into a deleted tenant", len(items)))
		}
	}
	if s.forgeConnections != nil {
		if items, err := s.forgeConnections.ListByTenant(tctx, teamID); err != nil {
			out = append(out, fmt.Sprintf("forge connections (unreadable: %v)", err))
		} else if len(items) > 0 {
			out = append(out, fmt.Sprintf("%d forge connection(s)", len(items)))
		}
	}
	if counter, ok := s.cfg.Store.(activeRunCounter); ok && counter != nil {
		if n, err := counter.CountActiveRunsByTenant(tctx, teamID); err != nil {
			out = append(out, fmt.Sprintf("active runs (unreadable: %v)", err))
		} else if n > 0 {
			out = append(out, fmt.Sprintf("%d active run(s)", n))
		}
	}
	if s.apiKeys != nil {
		if keys, err := s.apiKeys.ListByTeam(tctx, teamID, ""); err != nil {
			out = append(out, fmt.Sprintf("api keys (unreadable: %v)", err))
		} else if len(keys) > 0 {
			out = append(out, fmt.Sprintf("%d api key(s)", len(keys)))
		}
	}
	// Anything that still FIRES after the team is gone. A webhook is the
	// sharp one: its config is authenticated by its own token, so a delivery
	// keeps arriving for a tenant that no longer exists (the intake refuses
	// it now, but a surface that must be refused is one that should not have
	// been left behind).
	if s.webhookConfigs != nil {
		if hooks, err := s.webhookConfigs.ListByTenant(tctx, teamID); err != nil {
			out = append(out, fmt.Sprintf("webhooks (unreadable: %v)", err))
		} else if len(hooks) > 0 {
			out = append(out, fmt.Sprintf("%d inbound webhook(s)", len(hooks)))
		}
	}
	// Anything that still holds a CREDENTIAL. Left behind, these are sealed
	// secrets belonging to a tenant nothing can reach — the same leak the
	// org purge sweeper exists to prevent, arrived at by a different door.
	if s.genericSecrets != nil {
		if secs, err := s.genericSecrets.ListByTeam(tctx, teamID, ""); err != nil {
			out = append(out, fmt.Sprintf("secrets (unreadable: %v)", err))
		} else if len(secs) > 0 {
			out = append(out, fmt.Sprintf("%d named secret(s)", len(secs)))
		}
	}
	if s.oauthStore != nil {
		if recs, err := s.oauthStore.ListByUser(tctx, secrets.OrgOwnerKey(teamID)); err != nil {
			out = append(out, fmt.Sprintf("team forfaits (unreadable: %v)", err))
		} else if len(recs) > 0 {
			out = append(out, fmt.Sprintf("%d shared forfait connection(s)", len(recs)))
		}
	}
	if s.botSources != nil {
		if bots, err := s.botSources.ListByTenant(tctx, teamID); err != nil {
			out = append(out, fmt.Sprintf("team bots (unreadable: %v)", err))
		} else if len(bots) > 0 {
			out = append(out, fmt.Sprintf("%d team-authored bot(s)", len(bots)))
		}
	}
	return out
}

// handleDeleteTeam removes an EMPTY team. The reorg case it exists for is a
// team created by mistake — a wrong name, a duplicate — which until now was
// permanent.
func (s *Server) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	t, err := s.authStore().GetTeam(r.Context(), teamID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	if t.OrgID == "" || (!s.canManageOrg(r.Context(), id, t.OrgID) && !id.IsSuperAdmin) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	if t.Personal {
		httpError(w, http.StatusUnprocessableEntity,
			"a personal team is deleted with its org (`DELETE /api/admin/orgs/{id}`), which runs the purge cascade")
		return
	}
	if id.TeamID == teamID {
		httpError(w, http.StatusConflict, "cannot delete your active team — switch to another team first")
		return
	}
	if residue := s.teamResidue(r.Context(), teamID); len(residue) > 0 {
		httpError(w, http.StatusConflict,
			"team is not empty: %v — deleting it would strand this data under a tenant nothing can reach", residue)
		return
	}
	// Memberships first: a team row deleted while its grants survive leaves
	// members pointing at a team that no longer resolves, and a JWT carrying
	// it fails every gate with "not found" rather than "no longer a member".
	members, err := s.authStore().ListMembershipsByTeam(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list members: %v", err)
		return
	}
	for _, m := range members {
		if err := s.authStore().DeleteMembership(r.Context(), m.UserID, teamID); err != nil {
			httpError(w, http.StatusInternalServerError, "revoke membership %s: %v", m.UserID, err)
			return
		}
	}
	if err := s.authStore().DeleteTeam(r.Context(), teamID); err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditOrg(r, t.OrgID, "team.deleted", "team", teamID, map[string]any{"name": t.Name, "slug": t.Slug})
	w.WriteHeader(http.StatusNoContent)
}

// handlePutTeamMember places an EXISTING user in the team with a role,
// idempotently — the direct counterpart of the email invitation, for the
// case the invitation exists to solve and cannot: a user who already has an
// account on this instance.
//
// It grants no right the caller did not already hold: an org admin manages
// every team of their org and may already set any member's role, so
// requiring an email round trip only slowed a reorg down. The user must
// already be a member of the team's ORG — that membership is the identity
// boundary, and creating it silently here would let a team admin pull a
// stranger into their org.
func (s *Server) handlePutTeamMember(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	memberID := r.PathValue("user_id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	role := identity.Role(req.Role)
	if !role.Valid() {
		httpError(w, http.StatusBadRequest, "invalid role")
		return
	}
	t, err := s.authStore().GetTeam(r.Context(), teamID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	if t.Personal {
		httpError(w, http.StatusUnprocessableEntity, "a personal team takes no other member")
		return
	}
	u, err := s.authStore().GetUser(r.Context(), memberID)
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			httpError(w, http.StatusNotFound,
				"no such user — this endpoint places an EXISTING account; invite an unknown email with POST /api/teams/{id}/invitations")
			return
		}
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	if u.Status == identity.UserStatusDisabled {
		httpError(w, http.StatusUnprocessableEntity, "user %s is disabled — re-enable the account before granting it a team", u.Email)
		return
	}
	if t.OrgID != "" {
		if _, err := s.authStore().GetOrgMembership(r.Context(), memberID, t.OrgID); err != nil {
			if errors.Is(err, identity.ErrNotFound) {
				httpError(w, http.StatusUnprocessableEntity,
					"user %s is not a member of this team's organization — add them to the org first (an org membership is the identity boundary a team grant sits inside)", u.Email)
				return
			}
			httpError(w, http.StatusInternalServerError, "resolve org membership: %v", err)
			return
		}
	}
	now := time.Now().UTC()
	if err := s.authStore().UpsertMembership(r.Context(), identity.Membership{
		UserID: memberID, TeamID: teamID, Role: role, InvitedBy: id.UserID, JoinedAt: now,
	}); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "member.added", "member", memberID, map[string]any{"role": string(role), "email": u.Email})
	writeJSON(w, map[string]any{"user_id": memberID, "team_id": teamID, "role": string(role)})
}

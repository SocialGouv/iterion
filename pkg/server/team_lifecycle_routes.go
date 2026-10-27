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
	s.mux.Handle("PUT /api/teams/{id}/run-visibility", s.requireAuth(http.HandlerFunc(s.handleSetTeamRunVisibility)))
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
// this creates or updates.
//
// Super-admin only, unlike every other org roster write. The member_id is an
// account id the caller names rather than an address its owner answers at:
// under org admin it absorbs any account into the org at any role, discloses
// that account's email through the roster, and turns the 404/200 split into
// an existence oracle over arbitrary ids. An org admin adds someone by email
// invitation (which the invitee accepts) and keeps PATCH/DELETE over the
// members they already hold.
//
// It carries no rank guard of its own: requireRankOverMember exempts platform
// admins — they hold no org row for a ladder to place them on — so on a route
// only they reach, a call to it could never refuse.
func (s *Server) handlePutOrgMember(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	memberID := r.PathValue("user_id")
	if !s.platformAuthority(w, r) {
		return
	}
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
	// This write is idempotent on an EXISTING membership too, so it is also a
	// way an org role goes DOWN: the pending invitations the member minted
	// above the role just written are reconciled here as they are on the
	// roster's own PATCH.
	if !s.reconcileMintedInvitations(w, r, orgID, memberID, role, "placement") {
		return
	}
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

// handleSetTeamRunVisibility writes Team.RunVisibility (ADR-138): who
// beyond the team may READ its runs by id. Exposure of a team's run
// contents to the org is a governance act exactly like suspension — the
// same org gate, the same double audit — and resetting to team-private is
// the revocation half of the axis: the choke point reads the field LIVE,
// so closing it ends every org-shared read and passive socket within the
// standing tick. An org membership is the audience, not a team roster:
// removing a TEAM membership never revokes the sharing.
func (s *Server) handleSetTeamRunVisibility(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.orgAdminOfTeam(r.Context(), id, teamID) && !id.IsSuperAdmin {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	var req struct {
		RunVisibility *string `json:"run_visibility"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// A PUT without the field says nothing: refusing it keeps "" (the
	// reset) an explicit, audited act instead of a client's omission.
	if req.RunVisibility == nil {
		httpError(w, http.StatusBadRequest, `run_visibility is required (send "" to reset to team-private)`)
		return
	}
	if !identity.ValidRunVisibility(*req.RunVisibility) {
		httpError(w, http.StatusBadRequest, `invalid run_visibility (use "" or "org")`)
		return
	}
	t, err := s.authStore().PatchTeam(r.Context(), teamID, identity.TeamPatch{RunVisibility: req.RunVisibility})
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	meta := map[string]any{"run_visibility": *req.RunVisibility}
	s.auditTenant(r, teamID, "team.run_visibility_changed", "team", teamID, meta)
	if t.OrgID != "" {
		s.auditOrg(r, t.OrgID, "team.run_visibility_changed", "team", teamID, meta)
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
//
// The route is gated on canManageOrg (org admin or owner) or a platform
// admin, and the rank rule on top of that gate is the org ladder over every
// grant the deletion clears (requireRankOverTeamMembers): no author deletes a
// team whose roster holds a member their own standing does not cover. The
// refusals keep their precedence — 404 (no team), 403 (the gate), 422
// (personal team), 409 (the author's active team, the residue), the 500 of
// the members read the rule itself reuses — and the rank rule answers before
// the first revoke, so a refused deletion leaves the team and its grants
// untouched.
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
	// The rank rule, before the first revoke: the members read above serves
	// the rule and the act from one read.
	if !s.requireRankOverTeamMembers(w, r, id, t.OrgID, members) {
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
	if !s.requireOrgMembershipForGrant(w, r, t.OrgID, memberID) {
		return
	}
	if !s.requireRankOverMember(w, r, id, "", teamID, memberID, role, false) {
		return
	}
	now := time.Now().UTC()
	if err := s.authStore().UpsertMembership(r.Context(), identity.Membership{
		UserID: memberID, TeamID: teamID, Role: role, InvitedBy: id.UserID, JoinedAt: now,
	}); err != nil {
		// A write that reports an error may have LANDED: the compensation
		// owes this path too, and answers before the write error does.
		if !s.confirmOrgMembershipForGrant(w, r, t.OrgID, teamID, memberID, "placement", grantWriteFailed) {
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if !s.confirmOrgMembershipForGrant(w, r, t.OrgID, teamID, memberID, "placement", grantWriteAcked) {
		return
	}
	s.auditTenant(r, teamID, "member.added", "member", memberID, map[string]any{"role": string(role), "email": u.Email})
	// An idempotent placement can LOWER the role it re-writes, which is the
	// same authority-lowering write the PATCH is: the member's pending
	// invitations are measured against the cap that would bind their minting
	// NOW (the role just written), and the rows it refuses go.
	if !s.reconcileTeamRankInvitations(w, r, teamID, memberID, role, "placement") {
		return
	}
	writeJSON(w, map[string]any{"user_id": memberID, "team_id": teamID, "role": string(role)})
}

// grantCompensationTimeout bounds a compensation's store work on a context
// detached from the request, which has no deadline of its own. The grant
// guard takes one budget per REPAIR store call — the diagnosis, then the
// revoke — since a call that spends a shared one is exactly the case where
// what follows it still has to happen, while the off-boarding's second
// sweep takes one for its reads and its k writes, since those reads are the
// repair's input rather than a diagnosis. The delete of the membership row
// that FOLLOWS that sweep takes its own, for the reason the revoke does: a
// slow-but-working store would otherwise spend the sweep's budget and turn
// a complete off-boarding into a 500 naming a state that does not exist.
const grantCompensationTimeout = 5 * time.Second

// grantSubjectTimeout bounds the LABEL of a refusal — the address its
// subject answers at — which is not a repair and is not priced like one: it
// is read once the repair is over, and a store too sick to serve the
// diagnosis cannot serve the label either, so a repair-sized budget buys the
// account id grantSubject would have answered with anyway while holding the
// request for a third budget in series. A store slower than this names the
// id.
const grantSubjectTimeout = grantCompensationTimeout / 5

// grantWriteOutcome is what the store ANSWERED to a grant write, which the
// compensation cannot read back from the row: the two named values make the
// four call sites of the two writers say which path they are on.
type grantWriteOutcome bool

const (
	grantWriteAcked  grantWriteOutcome = true
	grantWriteFailed grantWriteOutcome = false
)

// grantNeedsOrgBoundary reports whether writing role as a team grant depends
// on the target's org membership. A capability OUTSIDE the ladder
// (config_editor, rank 0) is never mirrored up to one — ADR-078 decision 4,
// which identity.OrgRoleForTeamRole implements by answering "" and pkg/auth's
// grantOrgMembershipForTeam by writing nothing, since an org-member row would
// pass canViewOrg — so the invitation path creates exactly that grant with no
// boundary behind it, and a write that keeps a grant off the ladder has no
// boundary to confirm.
func grantNeedsOrgBoundary(role identity.Role) bool {
	return identity.OrgRoleForTeamRole(role) != ""
}

// grantSubject names the account a grant refusal is about: the address its
// owner answers at, or the account id when the user row cannot be read. The
// two writers reach the guards with different material in hand — the
// placement loads the user, a role change has no reason to — and one
// sentence naming an address on one writer and an opaque id on the other has
// two meanings. It is a LABEL, never a gate: no write is refused because the
// row carrying its subject's address could not be read.
func (s *Server) grantSubject(ctx context.Context, memberID string) string {
	if u, err := s.authStore().GetUser(ctx, memberID); err == nil && u.Email != "" {
		return u.Email
	}
	return memberID
}

// requireOrgMembershipForGrant refuses to write a team grant for a user who
// is not a member of the team's org: that membership is the identity
// boundary a grant sits inside, and creating it silently here would let a
// team admin pull a stranger into their org. A grant that already exists
// without one is the drift `GET /api/admin/users/{id}` reports as
// `orphan_grant` — named here too, never repaired behind the caller's back.
// orgID "" is a team with no parent org, which has no boundary to read.
//
// It answers the request and returns false when the grant must not be
// written. Both grant writers call it: the placement
// (PUT /api/teams/{id}/members/{user_id}) unconditionally, and the role
// change (PATCH /api/teams/{id}/members/{user_id}) for the roles that have a
// boundary at all (grantNeedsOrgBoundary).
func (s *Server) requireOrgMembershipForGrant(w http.ResponseWriter, r *http.Request, orgID, memberID string) bool {
	if orgID == "" {
		return true
	}
	if _, err := s.authStore().GetOrgMembership(r.Context(), memberID, orgID); err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			httpError(w, http.StatusUnprocessableEntity,
				"user %s is not a member of this team's organization — add them to the org first (an org membership is the identity boundary a team grant sits inside)",
				s.grantSubject(r.Context(), memberID))
			return false
		}
		httpError(w, http.StatusInternalServerError, "resolve org membership: %v", err)
		return false
	}
	return true
}

// confirmOrgMembershipForGrant re-reads the org membership AFTER the grant
// was written and takes the grant back unless that membership is still
// there. The read that authorized the write is a check taken BEFORE the
// act, and the two share no transaction: a
// DELETE /api/orgs/{id}/members/{user_id} landing in between revokes the
// user's grants and then the membership, leaving the grant written here
// alive with nothing behind it. A boundary that cannot be re-confirmed is
// not one, so an unreadable store takes the grant back too and the caller
// gets an error naming it rather than a success that did not hold. The
// off-boarding's own second sweep covers the opposite order.
//
// Both writers call it on BOTH outcomes of their write. A store that reports
// an error may have applied it anyway — UpsertMembership is a single
// UpdateOne, so an ack lost to an expiring deadline or a primary stepdown
// leaves the grant written and the call failed — and a grant that landed
// unacknowledged outlives the boundary exactly as a clean one does. There the
// compensation answers first: the membership being gone is why the write must
// not stand, where the write error would only send the caller back for the
// 422 the pre-check now owes them. The price is a revoke that may have had
// nothing to delete: DeleteMembership cannot tell a grant it removed from one
// that never landed, and a trail that stayed silent would hide the landed
// case.
//
// wrote is what the store answered, and it decides the UNREADABLE boundary
// alone. A membership definitively GONE takes the grant back on either
// outcome: no grant may outlive its boundary, whoever wrote it. A boundary
// that merely cannot be read takes back an ACKNOWLEDGED write, since nothing
// certifies the grant this request put there — but not a FAILED one: the
// store that could not serve the write is the store that cannot serve the
// read after it (one saturated pool, one stepdown), and revoking on that pair
// takes back the grant the member already held, which nothing the caller
// asked for authorizes. The caller then answers its own write error, and the
// grant that may have landed outside a boundary nobody could read is logged.
//
// The re-read and the revoke run on contexts DETACHED from the request: a
// client that goes away (an abort, a CLI ^C, a proxy read timeout) is the
// most ordinary reason a membership cannot be re-confirmed, and a revoke
// that dies with it leaves exactly the grant this guard exists to remove.
// Each gets a deadline of its own rather than a shared one: a store that
// answers the re-read only at the deadline (a Mongo stepdown, a saturated
// pool) is a store the revoke must still be attempted against, and a budget
// the diagnosis has already spent revokes nothing. The refusal's subject is
// read last, on a budget sized for a LABEL (grantSubjectTimeout), so it
// neither delays the repair it describes nor costs what the repair costs. A revoke that fails anyway is logged where it survives the
// caller, since the response it also names may reach nobody.
//
// act names what was in flight ("placement", "role change") for the error.
// It answers the request and returns false when the grant did not survive.
func (s *Server) confirmOrgMembershipForGrant(w http.ResponseWriter, r *http.Request, orgID, teamID, memberID, act string, wrote grantWriteOutcome) bool {
	if orgID == "" {
		return true
	}
	cctx, ccancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
	defer ccancel()
	_, err := s.authStore().GetOrgMembership(cctx, memberID, orgID)
	if err == nil {
		return true
	}
	gone := errors.Is(err, identity.ErrNotFound)
	if !gone && wrote == grantWriteFailed {
		if s.logger != nil {
			s.logger.Error("team grant %s/%s in org %s: the %s failed and the org membership could not be re-confirmed (%v) — a grant that landed unacknowledged would stand with no boundary read to check it",
				memberID, teamID, orgID, act, err)
		}
		return true
	}
	status := http.StatusInternalServerError
	// grant_unwound_ is what separates this revoke from the off-boarding's
	// own cascade (reason org_member_removed, orgs_routes.go): both delete a
	// team grant because the org membership went away, but this one also
	// means a write the caller issued was REFUSED and taken back, which is a
	// different event to the operator reading GET /api/teams/{id}/audit. The
	// suffix keeps the two store answers apart: definitively gone, or
	// unreadable.
	reasonCode := "grant_unwound_org_membership_unconfirmed"
	next := "place them in the team again once the organization membership can be read"
	if gone {
		status = http.StatusConflict
		reasonCode = "grant_unwound_org_member_removed"
		next = "add them to the organization again before placing them in the team"
	}
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
	defer rcancel()
	derr := s.authStore().DeleteMembership(rctx, memberID, teamID)
	// The subject is resolved AFTER the repair, on a budget of its own: the
	// diagnosis above may have spent cctx to its deadline — that incident is
	// why this guard exists — and a label read on a spent budget names an
	// opaque id, where one on the REQUEST's context has no deadline at all
	// and would hold the revoke behind a store that cannot answer who the
	// subject is. That budget is grantSubjectTimeout, not the repair's: the
	// store that answered neither the re-read nor the revoke would otherwise
	// hold the request for a third full budget to buy the id the fallback
	// already has.
	sctx, scancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantSubjectTimeout)
	defer scancel()
	subject := s.grantSubject(sctx, memberID)
	reason := fmt.Sprintf("could not re-confirm the organization membership of %s: %v", subject, err)
	if gone {
		reason = fmt.Sprintf("user %s was removed from this team's organization while the %s was in flight", subject, act)
	}
	if derr != nil {
		if s.logger != nil {
			// "may stand", as the body says: a delete that answers an error
			// may have applied it, and DeleteMembership cannot tell that
			// from one that reached nothing.
			s.logger.Error("team grant %s/%s may stand with no org membership in %s: %s, and revoking it failed: %v",
				memberID, teamID, orgID, reason, derr)
		}
		httpError(w, http.StatusInternalServerError,
			"%s — and revoking the team grant just written failed: %v; %s may hold a team grant with no org membership behind it",
			reason, derr, subject)
		return false
	}
	s.auditTenant(r, teamID, "member.removed", "member", memberID, map[string]any{"reason": reasonCode})
	// The grant just taken back is the premise the mint's own confirmation
	// keeps a self-addressed invitation on (standingAfterMintTakesInvitation),
	// and the rule takes the rows kept on it where the org membership row is
	// GONE — the fact that premise stood beside
	// (standingGoneTakesAddressedTo). This exit has both states: on the 409
	// the row is definitively gone, so whoever withdrew the premise takes
	// what rested on it, a row left there being a re-entry ticket into the
	// team whose grant was unwound; on the 500 the row could not be READ, so
	// the rule cannot decide, no sweep runs, and the body names the row that
	// may stand as the sibling exits of the same rule do
	// (sweepInvitationsKeptOnTeamGrant).
	//
	// The sweep runs on its own budget, as the reads above do: the revoke may
	// have spent the one they would otherwise share, and the sweep still has
	// to happen. Its failure names the row in the refusal rather than
	// changing the status: the caller's write is refused either way, and an
	// operator reading this body is the one who can revoke the row by hand.
	kept := ""
	if !gone {
		kept = fmt.Sprintf(", and a pending invitation %s addressed to their own address in this team may stand on it: whether the rule takes such a row rests on the org membership row this exit could not read (listed by GET /api/teams/%s/invitations, revoked by hand)",
			subject, teamID)
	}
	if gone {
		ictx, icancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
		defer icancel()
		if serr := s.revokeInvitationsKeptOnTeamGrant(ictx, requestActor(r), teamID, memberID, "creator_team_grant_unwound"); serr != nil {
			if s.logger != nil {
				s.logger.Error("team grant %s/%s was unwound but sweeping the pending invitations kept on it failed: %v — such a row stands on a grant that is gone and accepting it would seat them back in the team",
					memberID, teamID, serr)
			}
			kept = fmt.Sprintf(", and sweeping the pending invitations %s addressed to their own address in this team failed: %v — such a row stands on the grant just revoked (listed by GET /api/teams/%s/invitations, revoked by hand)",
				subject, serr, teamID)
		}
		// The rows the member MINTED go with the grant too: the cap that would
		// bind their minting NOW measures no grant (K.2), so every role their
		// standing does not cover is refused. It runs after the sweep above,
		// as the removal's own order does, and its failure is carried in a
		// clause of its own — the exit already has a status and a body, and
		// the residue is the caller's to revoke by hand, like the kept rows'.
		tctx, tcancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
		defer tcancel()
		if terr := s.takeTeamRankInvitations(tctx, requestActor(r), teamID, memberID, "", "creator_team_rank_lowered"); terr != nil {
			if s.logger != nil {
				s.logger.Error("team grant %s/%s was unwound but revoking the pending invitations %s minted above what now covers them failed: %v — such an invitation still carries a rank its creator no longer holds",
					memberID, teamID, memberID, terr)
			}
			kept += fmt.Sprintf(", and revoking the pending invitations they minted above what now covers them failed: %v — such an invitation still carries a rank its creator no longer holds (listed by GET /api/teams/%s/invitations, revoked by hand)",
				terr, teamID)
		}
	}
	httpError(w, status, "%s — the team grant was revoked%s; %s", reason, kept, next)
	return false
}

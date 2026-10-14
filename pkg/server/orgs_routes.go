package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/mail"
)

// registerOrgRoutes wires the org self-service surface: the org roster
// (members), org invitations, the org-scoped usage view, and the teams
// inside the org. Org-level reads require org membership (canViewOrg);
// mutations require org admin (canManageOrg). SSO + audit org routes are
// registered by org_sso_routes.go / audit_helper.go.
func (s *Server) registerOrgRoutes() {
	s.mux.Handle("GET /api/orgs/{id}", s.requireAuth(http.HandlerFunc(s.handleGetOrg)))
	s.mux.Handle("GET /api/orgs/{id}/members", s.requireAuth(http.HandlerFunc(s.handleListOrgMembers)))
	s.mux.Handle("PATCH /api/orgs/{id}/members/{user_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateOrgMember)))
	s.mux.Handle("DELETE /api/orgs/{id}/members/{user_id}", s.requireAuth(http.HandlerFunc(s.handleRemoveOrgMember)))
	s.mux.Handle("GET /api/orgs/{id}/invitations", s.requireAuth(http.HandlerFunc(s.handleListOrgInvitations)))
	s.mux.Handle("POST /api/orgs/{id}/invitations", s.requireAuth(http.HandlerFunc(s.handleCreateOrgInvitation)))
	s.mux.Handle("DELETE /api/orgs/{id}/invitations/{invite_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteOrgInvitation)))
	s.mux.Handle("GET /api/orgs/{id}/usage", s.requireAuth(http.HandlerFunc(s.handleOrgUsage)))
	s.mux.Handle("GET /api/orgs/{id}/teams", s.requireAuth(http.HandlerFunc(s.handleListOrgTeams)))
	s.mux.Handle("POST /api/orgs/{id}/teams", s.requireAuth(http.HandlerFunc(s.handleCreateOrgTeam)))
	s.mux.Handle("GET /api/orgs/{id}/settings", s.requireAuth(http.HandlerFunc(s.handleGetOrgSettings)))
	s.mux.Handle("PATCH /api/orgs/{id}/settings", s.requireAuth(http.HandlerFunc(s.handleUpdateOrgSettings)))
	s.mux.Handle("PATCH /api/orgs/{id}/teams/{team_id}/caps", s.requireAuth(http.HandlerFunc(s.handleUpdateOrgTeamCaps)))
}

// handleGetOrg is the missing twin of GET /api/teams/{id}: one org, read
// by anyone who may view it. Without it the only way to resolve an org by
// id was the caller's own identity tree, so a super-admin who is not a
// member of an org could not open its page at all — every API behind that
// page would have answered, since canViewOrg short-circuits on
// IsSuperAdmin.
//
// It serves toOrgView, quota fields included. That is not a new
// disclosure: GET /api/orgs/{id}/usage already embeds the same view behind
// the same canViewOrg gate.
func (s *Server) handleGetOrg(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canViewOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "not a member of this org")
		return
	}
	o, err := s.authStore().GetOrg(r.Context(), orgID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	writeJSON(w, toOrgView(o))
}

// orgSettingsView is the org-admin-managed slice of Org settings — the
// governance knobs an org runs itself, distinct from the super-admin
// plan/budget fields (/api/admin/orgs) which stay the platform's contract.
type orgSettingsView struct {
	RequireProvisionApproval bool `json:"require_provision_approval"`
	// ProvisionApprovalScope narrows what the flag above parks: every
	// request, or only the ones from teams with no credential of their own.
	ProvisionApprovalScope string `json:"provision_approval_scope"`
}

func (s *Server) handleGetOrgSettings(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canViewOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "not a member of this org")
		return
	}
	o, err := s.authStore().GetOrg(r.Context(), orgID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	writeJSON(w, orgSettingsView{
		RequireProvisionApproval: o.RequireProvisionApproval,
		ProvisionApprovalScope:   string(o.EffectiveProvisionApprovalScope()),
	})
}

func (s *Server) handleUpdateOrgSettings(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	var req struct {
		RequireProvisionApproval *bool   `json:"require_provision_approval,omitempty"`
		ProvisionApprovalScope   *string `json:"provision_approval_scope,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	patch := identity.OrgPatch{RequireProvisionApproval: req.RequireProvisionApproval}
	if req.ProvisionApprovalScope != nil {
		scope := identity.ProvisionApprovalScope(*req.ProvisionApprovalScope)
		if !identity.ValidProvisionApprovalScope(scope) {
			httpError(w, http.StatusBadRequest, "invalid provision_approval_scope (all|shared_credentials)")
			return
		}
		patch.ProvisionApprovalScope = &scope
	}
	// PATCH, not read-modify-write: the org settings, the credential
	// audience and the super-admin plan fields are three independent
	// editors of ONE document, and a whole-document replace lets each
	// silently revert the others.
	o, err := s.authStore().PatchOrg(r.Context(), orgID, patch)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditOrg(r, orgID, "org.settings_updated", "org", orgID, map[string]any{
		"require_provision_approval": o.RequireProvisionApproval,
		"provision_approval_scope":   string(o.EffectiveProvisionApprovalScope()),
	})
	writeJSON(w, orgSettingsView{
		RequireProvisionApproval: o.RequireProvisionApproval,
		ProvisionApprovalScope:   string(o.EffectiveProvisionApprovalScope()),
	})
}

// handleUpdateOrgTeamCaps lets an ORG admin set the per-team executor caps
// of the teams in THEIR org (MaxConcurrentRuns / LaunchRatePerMin) — the
// same fields the super-admin console manages, delegated one level down.
// The org-level monthly budget stays super-admin: an org does not raise
// its own plan.
func (s *Server) handleUpdateOrgTeamCaps(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	teamID := r.PathValue("team_id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	t, err := s.authStore().GetTeam(r.Context(), teamID)
	if err != nil || t.OrgID != orgID {
		httpError(w, http.StatusNotFound, "team not in org")
		return
	}
	var req struct {
		MaxConcurrentRuns *int `json:"max_concurrent_runs,omitempty"`
		LaunchRatePerMin  *int `json:"launch_rate_per_min,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// A PATCH, not a read-modify-write replace: the team row now carries
	// fields this route must never touch (the sovereign runner pool among
	// them), and writing back the read copy would resurrect whatever an
	// interleaved writer changed — or erase a field this build predates.
	var patch identity.TeamPatch
	if req.MaxConcurrentRuns != nil {
		var v int
		if !applyNonNegative(w, req.MaxConcurrentRuns, &v, "max_concurrent_runs") {
			return
		}
		vCopy := v
		patch.MaxConcurrentRuns = &vCopy
	}
	if req.LaunchRatePerMin != nil {
		var v int
		if !applyNonNegative(w, req.LaunchRatePerMin, &v, "launch_rate_per_min") {
			return
		}
		vCopy := v
		patch.LaunchRatePerMin = &vCopy
	}
	// The platform default is a CEILING for the delegated route: any non-zero
	// team value overrides it in the launch gate (orValue), so without this
	// clamp an org admin could raise a team above the deployment-wide limit.
	// Raising past the ceiling stays super-admin (/api/admin/orgs). 0 keeps
	// meaning "inherit the platform default" and is always allowed. Only the
	// fields THIS request submitted are validated — a stored super-admin
	// value above the ceiling must not lock the row against edits of the
	// other field.
	if d := s.orgDefaults.MaxConcurrentRuns; patch.MaxConcurrentRuns != nil && d > 0 && *patch.MaxConcurrentRuns > d {
		httpError(w, http.StatusUnprocessableEntity,
			"max_concurrent_runs %d exceeds the platform ceiling %d — a platform admin must raise it", *patch.MaxConcurrentRuns, d)
		return
	}
	if d := s.orgDefaults.LaunchRatePerMin; patch.LaunchRatePerMin != nil && d > 0 && *patch.LaunchRatePerMin > d {
		httpError(w, http.StatusUnprocessableEntity,
			"launch_rate_per_min %d exceeds the platform ceiling %d — a platform admin must raise it", *patch.LaunchRatePerMin, d)
		return
	}
	updated, err := s.authStore().PatchTeam(r.Context(), teamID, patch)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditOrg(r, orgID, "team.caps_updated", "team", teamID, map[string]any{
		"max_concurrent_runs": updated.MaxConcurrentRuns, "launch_rate_per_min": updated.LaunchRatePerMin,
	})
	writeJSON(w, toTeamSummaryView(updated))
}

type orgMemberView struct {
	UserID string `json:"user_id"`
	Email  string `json:"email,omitempty"`
	Name   string `json:"name,omitempty"`
	Role   string `json:"role"`
}

func (s *Server) handleListOrgMembers(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canViewOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "not a member of this org")
		return
	}
	mems, err := s.authStore().ListOrgMembershipsByOrg(r.Context(), orgID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list members: %v", err)
		return
	}
	out := make([]orgMemberView, 0, len(mems))
	for _, m := range mems {
		u, _ := s.authStore().GetUser(r.Context(), m.UserID)
		out = append(out, orgMemberView{UserID: m.UserID, Email: u.Email, Name: u.Name, Role: string(m.Role)})
	}
	writeJSON(w, struct {
		Members []orgMemberView `json:"members"`
	}{Members: out})
}

func (s *Server) handleUpdateOrgMember(w http.ResponseWriter, r *http.Request) {
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
	mb, err := s.authStore().GetOrgMembership(r.Context(), memberID, orgID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	mb.Role = role
	if err := s.authStore().UpsertOrgMembership(r.Context(), mb); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditOrg(r, orgID, "org_member.role_changed", "member", memberID, map[string]any{"role": string(role)})
	writeJSON(w, mb)
}

func (s *Server) handleRemoveOrgMember(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	memberID := r.PathValue("user_id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	// Cascade: removing a user from the org also revokes their access to
	// every team within it (you cannot hold a team grant without an org
	// membership) — the grants they hold and the pending invitations that
	// would re-create them. A failure of the FIRST pass aborts before the
	// org membership is touched, so the member keeps it — but the pass
	// revokes row by row with no transaction behind it, so what it got
	// through stays revoked and the member is PARTIALLY off-boarded, which
	// is what its error has to say. A failure of the SECOND leaves the
	// member removed — the last delete below runs on that path too, so the
	// membership row is taken either way — and possibly still holding
	// access written inside the window. A failure of the LAST delete of the
	// membership row leaves them an org member again if an accept or a
	// placement wrote it back, and an answer that meets both failures names
	// both. Both passes and both deletes are idempotent, and the cascade
	// acts on the id the path names whether or not an account answers to
	// it, so re-issuing the removal finishes whichever one stopped.
	if err := s.revokeOrgTeamAccess(r.Context(), r, orgID, memberID); err != nil {
		// Said where it survives the caller, as the detached tail's two
		// failures are: this exit and the next run on the REQUEST's context,
		// so a client that went away is the ordinary way they are reached —
		// and the body is then the only account of a half-off-boarded
		// member. No audit row covers them either: org_member.removed is
		// written once the membership row is gone.
		if s.logger != nil {
			s.logger.Error("org %s: revoking the team access of %s failed: %v — their org membership stands and the grants and invitations revoked before the failure stay revoked, so they are partially off-boarded; re-issue the removal",
				orgID, memberID, err)
		}
		httpError(w, http.StatusInternalServerError,
			"revoking the team access of %s in org %s failed: %s — their org membership stands, and the team grants and pending invitations revoked before the failure stay revoked (each one named in its team's audit trail); re-issue the removal to finish it",
			memberID, orgID, err.Error())
		return
	}
	if err := s.authStore().DeleteOrgMembership(r.Context(), memberID, orgID); err != nil {
		// The exit between the two passes, and the one that reads as "nothing
		// happened" unless it says otherwise: the first pass went through, so
		// the grants it found are revoked, and DeleteOne answers nil when
		// nothing matched — an error here can also be a delete whose ack was
		// lost. Re-issuing settles both halves.
		//
		// The invitation half carries the condition the pass itself has: an
		// invitation names an ADDRESS, so a target whose account row cannot
		// be read has none to select by and that half is skipped
		// (revokeOrgTeamAccess). A body that promised it outright would send
		// the operator who re-issues away from a live invitation.
		if s.logger != nil {
			s.logger.Error("org %s: removing the org membership of %s failed: %v — their team grants are revoked and the membership row may or may not be gone; re-issue the removal",
				orgID, memberID, err)
		}
		httpError(w, mapAuthErrorStatus(err),
			"removing the org membership of %s in org %s failed: %s — their team grants in this org are revoked (and the pending invitations addressed to them, where their account row named an address), and the membership row may or may not be gone; re-issue the removal to finish it",
			memberID, orgID, err.Error())
		return
	}
	s.auditOrg(r, orgID, "org_member.removed", "member", memberID, nil)
	// Second pass, now that the membership is gone: a placement
	// (PUT /api/teams/{id}/members/{user_id}) or a role change
	// (PATCH …/members/{user_id}) that read the membership while it was
	// still live can write its grant after the sweep above, and a team admin
	// can mint an invitation in the same window; none of those handlers
	// shares a transaction with this one. Sweeping again is what such a row
	// meets; the grant writers' own re-read covers the opposite order.
	//
	// It runs on a context detached from the request: the member row is
	// already gone, so a client that goes away here would otherwise leave
	// exactly the access this pass exists to revoke.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
	defer cancel()
	sweepErr := s.revokeOrgTeamAccess(cctx, r, orgID, memberID)
	if sweepErr != nil && s.logger != nil {
		s.logger.Error("org %s: %s was removed but re-sweeping their team access failed: %v — they may still hold a grant or a pending invitation in this org",
			orgID, memberID, sweepErr)
	}
	// The boundary row itself, once more and last: an accept whose
	// invitation this removal revoked may have CLAIMED it first (pkg/auth
	// claimInvitation) and written the org membership back afterwards, and
	// nothing orders that write against the sweep above. Deleting it again
	// takes such a row — and, being blind to who wrote it, any membership a
	// placement (PUT /api/orgs/{id}/members/{user_id}) landed inside the
	// window too, which answers 200 and leaves nothing behind.
	//
	// It runs whether or not the sweep above succeeded: the boundary row is
	// the one residual a failed sweep can still have its caller take, it is
	// what makes that failure's own "was removed" true, and the state it
	// would otherwise leave is the most invisible of them all — an org
	// member again, reading the org, with no drift flag on the grant since
	// the boundary is back.
	//
	// What it bounds is the WINDOW, not the race: an accept that claimed
	// before the first sweep and lands BOTH its access writes after this
	// delete keeps the grant and the boundary, and orphan_grant stays quiet
	// because the boundary is there — re-issuing the removal is the repair,
	// and closing it takes a conditional boundary write (a pkg/identity
	// seam) rather than one more sweep.
	//
	// It gets its OWN budget: the sweep above may have spent the one it
	// would otherwise share, and a repair that still has to happen is
	// exactly the call a spent budget must not kill
	// (grantCompensationTimeout). No audit line: DeleteOne answers nil
	// whether it took a resurrected row or found nothing, so a line here
	// would name an event this call cannot see.
	lctx, lcancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
	defer lcancel()
	lastErr := s.authStore().DeleteOrgMembership(lctx, memberID, orgID)
	if lastErr != nil && s.logger != nil {
		s.logger.Error("org %s: %s was removed but re-deleting their org membership failed: %v — an org membership written inside the window may stand",
			orgID, memberID, lastErr)
	}
	switch {
	case sweepErr != nil && lastErr != nil:
		httpError(w, http.StatusInternalServerError,
			"the org membership of %s was removed but re-sweeping their team access failed: %s, and re-deleting their org membership failed too: %s — they may still hold a team grant or a pending invitation in this org, and an org membership written while the removal was in flight may stand, so they would be an org member again; re-issue the removal",
			memberID, sweepErr.Error(), lastErr.Error())
	case sweepErr != nil:
		httpError(w, http.StatusInternalServerError,
			"the org membership of %s was removed but re-sweeping their team access failed: %s — they may still hold a team grant or a pending invitation in this org; re-issue the removal",
			memberID, sweepErr.Error())
	case lastErr != nil:
		httpError(w, http.StatusInternalServerError,
			"the team grants of %s in org %s are revoked (with the pending invitations addressed to them, where their account row named an address) but re-deleting their org membership failed: %s — an org membership written while the removal was in flight may stand, and they would be an org member again; re-issue the removal",
			memberID, orgID, lastErr.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// revokeOrgTeamAccess deletes everything that gives userID access to a team
// of orgID — the grants they hold and, where the account's own address can be
// read, the invitations addressed to them that nobody has accepted yet — and
// records one audit line per row actually revoked. It re-reads the org's
// teams, the user's grants and each team's invitations on each call, so a
// team, a grant or an invitation created since a previous call is covered. Re-running it revokes nothing twice:
// DeleteMembership answers nil when no row matched, and an invitation some
// other caller revoked in between is skipped on its ErrNotFound.
//
// A pending invitation is access the same way a grant is: accepting one
// writes the team grant AND, for any role on the org ladder, the org
// membership that was just deleted (pkg/auth grantOrgMembershipForTeam), so
// an off-boarding that swept only the grants is undone by one request, with
// no race to win.
//
// The invitations go FIRST in each pass because an accept does the same two
// things in the same order: it CLAIMS the invitation before it writes any
// access (pkg/auth claimInvitation). With both sides ordered alike, an
// accept survives a pass only by claiming before that pass's first delete
// AND writing its grant after its last — it has to straddle the whole pass,
// where the opposite order leaves it a single step to slip through. What a
// straddling accept then meets is the removal's last delete of the
// membership row: an accept whose boundary write lands BEFORE that delete
// keeps a grant with no boundary, which the console reports, and one that
// lands both its writes after it keeps the boundary too — a row the console
// carries no drift flag on at all (handleRemoveOrgMember).
//
// Not-yet-accepted is deliberately wider than the accept paths' "still
// acceptable": matching their clock here would be a second predicate to keep
// in step with theirs, and taking a row that was already dead costs nothing.
// An ACCEPTED invitation stays — it is the trail of how the member joined.
//
// The store work runs on ctx, which the caller chooses: a sweep that must
// outlive the request passes one detached from it. r names the audit actor.
func (s *Server) revokeOrgTeamAccess(ctx context.Context, r *http.Request, orgID, userID string) error {
	teams, err := s.authStore().ListTeamsByOrg(ctx, orgID)
	if err != nil {
		return fmt.Errorf("list org teams: %w", err)
	}
	// An invitation names an ADDRESS, not an account, so the sweep needs the
	// one this account answers at — and it must read it exactly as the accept
	// paths do (identity.Invitation.AddressedTo), or it leaves behind rows
	// they would honour. A user row that cannot be read stops the cascade:
	// an off-boarding that skipped the invitations on a row that may exist
	// would report a cascade it did not finish.
	//
	// An account that definitively has NO row (ErrNotFound) is the other
	// case, and it is not an error here: there is no address an invitation
	// could be addressed to, and no accept could turn one into access for
	// THIS id — a registration mints a new account id (pkg/auth
	// registerWithInvitation) and the existing-user accept logs in as this
	// account. The caller names an id the request path gave it, which this
	// endpoint neither requires to be a member nor refuses when unknown, so
	// a fatal read there would leave the grants AND the membership row it
	// exists to take. The invitation half is skipped, logged, and the rest
	// of the cascade runs.
	//
	// The line reports what this read found, never what the removal will
	// do: it is emitted before the grant sweep and before any delete of the
	// membership row, so this pass can still fail with everything standing
	// — and the caller's own answer is what says which state it left.
	u, err := s.authStore().GetUser(ctx, userID)
	addressKnown := err == nil
	if err != nil && !errors.Is(err, identity.ErrNotFound) {
		return fmt.Errorf("resolve the address invitations of %s are addressed to: %w", userID, err)
	}
	if !addressKnown && s.logger != nil {
		s.logger.Warn("org %s: %s has no account row, so no address their pending invitations could be addressed to — this sweep skips that half, and any invitation addressed to them stands, listed by GET /api/orgs/%s/invitations",
			orgID, userID, orgID)
	}
	if addressKnown {
		for _, t := range teams {
			invs, err := s.authStore().ListInvitationsByTeam(ctx, t.ID)
			if err != nil {
				return fmt.Errorf("list team %s invitations: %w", t.ID, err)
			}
			for _, inv := range invs {
				if inv.AcceptedAt != nil || !inv.AddressedTo(u.Email) {
					continue
				}
				if err := s.authStore().DeleteInvitation(ctx, inv.ID); err != nil {
					// A row another caller revoked between the listing and
					// here is already in the state this sweep wants, and both
					// stores say so definitively — where DeleteMembership
					// cannot tell a grant it removed from one that was never
					// there. So: no error, and no audit line for a row this
					// pass did not take.
					if errors.Is(err, identity.ErrNotFound) {
						continue
					}
					return fmt.Errorf("revoke team %s invitation: %w", t.ID, err)
				}
				s.auditTenant(r, t.ID, "invitation.deleted", "invitation", inv.ID, map[string]any{"email": inv.Email, "reason": "org_member_removed"})
			}
		}
	}
	inOrg := make(map[string]struct{}, len(teams))
	for _, t := range teams {
		inOrg[t.ID] = struct{}{}
	}
	held, err := s.authStore().ListMembershipsByUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("list team grants of %s: %w", userID, err)
	}
	for _, mb := range held {
		if _, ok := inOrg[mb.TeamID]; !ok {
			continue
		}
		if err := s.authStore().DeleteMembership(ctx, userID, mb.TeamID); err != nil {
			return fmt.Errorf("revoke team %s membership: %w", mb.TeamID, err)
		}
		s.auditTenant(r, mb.TeamID, "member.removed", "member", userID, map[string]any{"reason": "org_member_removed"})
	}
	return nil
}

func (s *Server) handleListOrgInvitations(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	// An org invitation is a team invitation against a team in the org;
	// list the union across the org's teams. This list is how the
	// invitation a mint left pending is found to be revoked, so a read
	// that failed answers the store's error rather than the empty list of
	// an org with nothing pending.
	teams, err := s.authStore().ListTeamsByOrg(r.Context(), orgID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "resolve the org's teams: %v", err)
		return
	}
	out := make([]identity.Invitation, 0)
	for _, t := range teams {
		invs, err := s.authStore().ListInvitationsByTeam(r.Context(), t.ID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "list the invitations of team %s: %v", t.ID, err)
			return
		}
		out = append(out, invs...)
	}
	writeJSON(w, struct {
		Invitations []identity.Invitation `json:"invitations"`
	}{Invitations: out})
}

func (s *Server) handleCreateOrgInvitation(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	var req struct {
		Email  string `json:"email"`
		Role   string `json:"role"`
		TeamID string `json:"team_id,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	role := identity.Role(req.Role)
	if !role.Valid() {
		httpError(w, http.StatusBadRequest, "invalid role")
		return
	}
	// Invitations join a team (which grants org membership on accept).
	// Default to the org's first non-personal team when none is given.
	teamID := req.TeamID
	if teamID == "" {
		primary, err := s.firstTeamInOrg(r.Context(), orgID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "resolve the org's teams: %v", err)
			return
		}
		teamID = primary
	}
	if teamID == "" {
		httpError(w, http.StatusBadRequest, "org has no team to invite into")
		return
	}
	// The team the invitation joins comes from the REQUEST, where canManageOrg
	// authorized the org of the PATH and nothing about another one — the same
	// confirmation the org-scoped caps PATCH and the invitation DELETE owe,
	// in their words: a team of another org and a team that is nowhere are
	// one answer, since telling them apart tells an org admin which ids
	// exist outside their own tenant. Accepting such an invitation writes a
	// team grant, and on the ladder an org membership, inside a tenant this
	// caller does not manage.
	t, err := s.authStore().GetTeam(r.Context(), teamID)
	if err != nil && !errors.Is(err, identity.ErrNotFound) {
		httpError(w, http.StatusInternalServerError, "read the team: %v", err)
		return
	}
	if err != nil || t.OrgID != orgID {
		httpError(w, http.StatusNotFound, "team not in org")
		return
	}
	if !s.requireOrgStandingToInvite(w, r, id, t.OrgID, role) {
		return
	}
	tok, inv, err := s.authSvc.CreateInvitation(r.Context(), teamID, req.Email, role, id.UserID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditOrg(r, orgID, "org_invitation.created", "invitation", inv.ID, map[string]any{"email": inv.Email, "role": string(inv.Role)})
	if s.authSvc.EmailEnabled() {
		org, oerr := s.authStore().GetOrg(r.Context(), orgID)
		orgName := orgID
		if oerr == nil {
			orgName = org.Name
		}
		msg := mail.RenderInvitation(inv.Email, mail.InviteData{
			TeamName:  orgName,
			Role:      string(inv.Role),
			AcceptURL: s.authSvc.PublicURL() + "/invitations/accept?token=" + tok,
			InvitedBy: id.Email,
		})
		s.goSafe("org-invitation-email", func() {
			bg, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := s.authSvc.Mailer().Send(bg, msg); err != nil && s.logger != nil {
				s.logger.Warn("auth: org invitation email to %s: %v", msg.To, err)
			}
		})
	}
	writeJSON(w, struct {
		ID        string    `json:"id"`
		Token     string    `json:"token"`
		Email     string    `json:"email"`
		Role      string    `json:"role"`
		TeamID    string    `json:"team_id"`
		ExpiresAt time.Time `json:"expires_at"`
	}{ID: inv.ID, Token: tok, Email: inv.Email, Role: string(inv.Role), TeamID: teamID, ExpiresAt: inv.ExpiresAt})
}

func (s *Server) handleDeleteOrgInvitation(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	inviteID := r.PathValue("invite_id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	inv, err := s.authStore().GetInvitation(r.Context(), inviteID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	// Confirm the invitation's team belongs to this org.
	if t, terr := s.authStore().GetTeam(r.Context(), inv.TeamID); terr != nil || t.OrgID != orgID {
		httpError(w, http.StatusNotFound, "invitation not in org")
		return
	}
	if err := s.authStore().DeleteInvitation(r.Context(), inviteID); err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditOrg(r, orgID, "org_invitation.deleted", "invitation", inviteID, map[string]any{"email": inv.Email})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleOrgUsage(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canViewOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "not a member of this org")
		return
	}
	o, err := s.authStore().GetOrg(r.Context(), orgID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	writeJSON(w, s.buildOrgUsageView(r.Context(), s.authStore(), o))
}

func (s *Server) handleListOrgTeams(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canViewOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "not a member of this org")
		return
	}
	teams, err := s.authStore().ListTeamsByOrg(r.Context(), orgID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	views := make([]teamSummaryView, 0, len(teams))
	for _, t := range teams {
		views = append(views, toTeamSummaryView(t))
	}
	writeJSON(w, struct {
		Teams []teamSummaryView `json:"teams"`
	}{Teams: views})
}

func (s *Server) handleCreateOrgTeam(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	var req createTeamReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name required")
		return
	}
	t, err := s.authSvc.CreateTeamFor(r.Context(), id.UserID, orgID, req.Name, req.Slug)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditOrg(r, orgID, "team.created", "team", t.ID, map[string]any{"name": t.Name})
	writeJSON(w, toTeamSummaryView(t))
}

// primaryTeamOfOrgOrFail resolves the org's primary team for the org routes
// whose resource stays team-keyed (ADR-048) and ANSWERS the request when it
// cannot: **500** with the store's error when the org's teams cannot be listed
// — a read that failed is not an org without a team — and **404** when the org
// really holds none. It returns false once it has answered, so the caller
// returns without touching the tenant store. The invitation mint resolves the
// same team through firstTeamInOrg directly: an org with no team to invite
// into is a 400 on that route, not the 404 of a missing SSO tenant.
func (s *Server) primaryTeamOfOrgOrFail(w http.ResponseWriter, r *http.Request, orgID string) (string, bool) {
	teamID, err := s.firstTeamInOrg(r.Context(), orgID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "resolve the org's teams: %v", err)
		return "", false
	}
	if teamID == "" {
		httpError(w, http.StatusNotFound, "org has no team")
		return "", false
	}
	return teamID, true
}

// firstTeamInOrg returns the org's "primary team": the first non-personal
// team, else any team. It is the **storage tenant** for org-level resources
// that stay team-keyed by design — SSO providers/domains and the team an
// org invitation joins — so the org REST surface never has to re-key those
// stores (ADR-048 "As built"). Returns "" when the org has no team, and the
// store's error when its teams cannot be LISTED: a read that failed is not
// an org without a team, and every caller but the non-oracle SSO discovery
// (resolveOrgTenants) answers it rather than falling back.
func (s *Server) firstTeamInOrg(ctx context.Context, orgID string) (string, error) {
	teams, err := s.authStore().ListTeamsByOrg(ctx, orgID)
	if err != nil {
		return "", err
	}
	if len(teams) == 0 {
		return "", nil
	}
	for _, t := range teams {
		if !t.Personal {
			return t.ID, nil
		}
	}
	return teams[0].ID, nil
}

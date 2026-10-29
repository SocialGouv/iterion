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
	// The mint is gated exactly like the team-scoped twin
	// (auth.MintTeamToken — see auth_routes.go).
	s.mux.Handle("POST /api/orgs/{id}/invitations", s.requireAuth(s.gateDerivation(auth.MintTeamToken, http.HandlerFunc(s.handleCreateOrgInvitation))))
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
		s.identityFailure(w, r, fmt.Errorf("list the members of org %s: %w", orgID, err))
		return
	}
	userIDs := make([]string, 0, len(mems))
	for _, m := range mems {
		userIDs = append(userIDs, m.UserID)
	}
	// The team roster's twin (auth_teams.go handleListTeamMembers): one bulk
	// fetch, and a user absent from the map yields the zero User — the row
	// still renders with id + role, because the read ANSWERED that the
	// account is gone. A read that FAILED is not that: rendering it the same
	// way showed every member as an account with no email or name, 200.
	usersByID, err := s.authStore().GetUsersByIDs(r.Context(), userIDs)
	if err != nil {
		s.identityFailure(w, r, fmt.Errorf("load the member accounts of org %s: %w", orgID, err))
		return
	}
	out := make([]orgMemberView, 0, len(mems))
	for _, m := range mems {
		u := usersByID[m.UserID]
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
	if !s.requireOrgStandingToWriteOrgRole(w, r, id, orgID, role) {
		return
	}
	mb, err := s.authStore().GetOrgMembership(r.Context(), memberID, orgID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	if !s.requireRankOverMember(w, r, id, orgID, "", memberID, "", false) {
		return
	}
	mb.Role = role
	if err := s.authStore().UpsertOrgMembership(r.Context(), mb); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditOrg(r, orgID, "org_member.role_changed", "member", memberID, map[string]any{"role": string(role)})
	if !s.reconcileMintedInvitations(w, r, orgID, memberID, role, "role change") {
		return
	}
	writeJSON(w, mb)
}

// reconcileMintedInvitations revokes the pending invitations memberID minted
// in orgID above the org role just written for them: the role written is also
// the standing those invitations were capped at, and one above it would still
// hand out a rank its creator no longer holds, since the accept reads nothing
// about them (requireRankToInvite caps at the mint). Where the standing
// written falls below admin, the sweep reads the grant they hold in each team
// and takes the roles above it too — the cap that would bind their minting
// now measures both (revokeInvitationsAboveOrgStanding).
//
// It runs AFTER the write, on a context detached from the request: the
// standing is already lowered, so a mint racing in is capped by the new one —
// the cap reads the row — and a client that goes away here would otherwise
// leave exactly the invitations this sweep exists to revoke. A sweep that
// could not read them is not one that found none, so it answers 500 naming
// the act to re-issue; the sweep reconciles against the role WRITTEN rather
// than against the one it replaced, so re-issuing the same act finishes it.
//
// act names what the caller did, for the body that asks for it again
// ("role change", "placement"). It answers the request and returns false when
// it did.
func (s *Server) reconcileMintedInvitations(w http.ResponseWriter, r *http.Request, orgID, memberID string, role identity.OrgRole, act string) bool {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
	defer cancel()
	err := s.revokeInvitationsAboveOrgStanding(cctx, r, orgID, memberID, role)
	if err == nil {
		return true
	}
	if s.logger != nil {
		s.logger.Error("org %s: the org role of %s is now %s but revoking the pending invitations they created above that standing failed: %v — such an invitation still carries a rank its creator no longer holds; re-issue the %s",
			orgID, memberID, role, err, act)
	}
	httpError(w, http.StatusInternalServerError,
		"the org role of %s in org %s is now %s but revoking the pending invitations they created above that standing failed: %s — such an invitation still carries a rank its creator no longer holds (listed by GET /api/orgs/%s/invitations, revoked by hand); re-issue the %s to finish it",
		memberID, orgID, role, err.Error(), orgID, act)
	return false
}

// The invitation residual this cascade reports, in the two voices its bodies
// need: what a pass that went through revoked, and what one that failed may
// have left. The minted half names the invitations that carry an ORG ROLE,
// which is what the sweep takes (orgMirrorAbove against
// orgStandingAfterRemoval): the role mirrored up to nothing (config_editor,
// ADR-078) outranks no standing and stands, so no body may report it revoked,
// nor send an operator re-issuing the removal looking for it — unless it is
// addressed to the member themselves, which is the addressed-to clause these
// strings carry beside the minted half (standingTakesInvitation).
const (
	revokedMintedInvitations = "the pending invitations they minted here that carry an org role (and those addressed to them, where their account row named an address)"
	standingMintedInvitation = "a pending invitation they minted here that carries an org role (and one addressed to them, where their account row names an address)"
)

func (s *Server) handleRemoveOrgMember(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	memberID := r.PathValue("user_id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	// Before the cascade below, whose first pass already writes the target's
	// grants.
	if !s.requireRankOverMember(w, r, id, orgID, "", memberID, "", false) {
		return
	}
	// Cascade: removing a user from the org also revokes their access to
	// every team within it (you cannot hold a team grant without an org
	// membership) — the grants they hold, the pending invitations that would
	// re-create them, and the ones they minted, which would seat somebody
	// else on a standing they no longer hold. A failure of the FIRST pass aborts before the
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
		// The ADDRESSED-TO half carries the condition the pass itself has: an
		// invitation names an ADDRESS, so a target whose account row cannot
		// be read has none to select by and that half alone is skipped
		// (revokeOrgTeamAccess) — the invitations the target MINTED are
		// selected by the id the path gave and are swept either way. A body
		// that promised the addressed-to half outright would send the
		// operator who re-issues away from a live invitation.
		if s.logger != nil {
			s.logger.Error("org %s: removing the org membership of %s failed: %v — their team grants are revoked and the membership row may or may not be gone; re-issue the removal",
				orgID, memberID, err)
		}
		httpError(w, mapAuthErrorStatus(err),
			"removing the org membership of %s in org %s failed: %s — their team grants in this org are revoked, with "+revokedMintedInvitations+", and the membership row may or may not be gone; re-issue the removal to finish it",
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
	// meets; the opposite order is covered by those handlers' own re-reads
	// after their write — the grant writers' (confirmOrgMembershipForGrant)
	// and the mint's (confirmOrgStandingAfterMint), whose self-addressed
	// case rests on a grant this pass revokes, which is what the last
	// invitation sweep below settles.
	//
	// It runs on a context detached from the request: the member row is
	// already gone, so a client that goes away here would otherwise leave
	// exactly the access this pass exists to revoke.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
	defer cancel()
	sweepErr := s.revokeOrgTeamAccess(cctx, r, orgID, memberID)
	if sweepErr != nil && s.logger != nil {
		s.logger.Error("org %s: %s was removed but re-sweeping their team access failed: %v — they may still hold a grant in this org, or "+standingMintedInvitation,
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
	// The invitations once more, and LAST of every write this removal makes.
	// Inside a pass the invitation sweep precedes the grant revoke
	// (revokeOrgTeamAccess), and that grant is the premise the mint's own
	// confirmation keeps a self-addressed row on
	// (standingAfterMintTakesInvitation): a row minted between the two is
	// seen by neither sweep and kept on a grant the pass takes one store
	// call later, which leaves the removed member a re-entry ticket into a
	// team of the org — their own invitation at a role carrying no org rank,
	// with no grant and no membership row left. Selecting once more, with no
	// write of this removal after it, is what makes the 204 true of this
	// removal's own writes: no invitation row this rule takes stands behind
	// it on a premise this handler withdrew.
	//
	// What it does not bound is another writer: a grant or a boundary row
	// written back AFTER this selection's listing — the straddling accept
	// above, a team admin's PATCH, a placement — re-opens the window, and a
	// mint landing in it is kept by its own confirmation on the fact that
	// came back. That state is reported (orphan_grant, off_ladder; the row
	// is listed by GET /api/orgs/{id}/invitations), and whoever withdraws
	// that premise next takes the row with it: the team-member DELETE and
	// the grant compensation sweep the rows kept on a grant they revoke
	// (revokeInvitationsKeptOnTeamGrant), and re-issuing the removal is the
	// repair for a boundary row.
	//
	// It runs whichever of the steps above failed — a residual invitation is
	// precisely what their bodies have to promise or deny — on its own
	// budget (grantCompensationTimeout), since both calls above may have
	// spent theirs. Its failure is carried in a clause of its OWN rather
	// than joined into the grant sweep's: on the exit where only this
	// selection failed, the second pass revoked every grant, so a body
	// blaming "their team access" would send an operator after access that
	// is gone — and the body that reports the grants revoked "with the
	// pending invitations they minted here" is never the one a failed
	// invitation sweep answers with.
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
	defer fcancel()
	lastSweepErr := s.revokeOrgTeamInvitations(fctx, r, orgID, memberID)
	if lastSweepErr != nil && s.logger != nil {
		s.logger.Error("org %s: %s was removed but the last sweep of their pending invitations failed: %v — "+standingMintedInvitation+" may stand",
			orgID, memberID, lastSweepErr)
	}
	// Each step that failed names what IT may have left, in a clause of its
	// own. The three fail independently and leave different residues, so a
	// body that routed one into another's words would send an operator
	// after access that is gone: on the exit where only the LAST sweep
	// failed, the second pass revoked every grant, and the one clause that
	// is true there is the invitation's. The exits of a failed grant pass
	// hold the mirror of that rule: the last sweep runs AFTER them, so the
	// invitation residue is that sweep's to report — carried here only when
	// IT failed (lastSweepResidue) — and a body naming it anyway would send
	// an operator after a row the sweep behind it took.
	lastSweepResidue := ""
	if lastSweepErr != nil {
		lastSweepResidue = fmt.Sprintf("the last sweep of their pending invitations failed: %s — "+standingMintedInvitation+" may stand (listed by GET /api/orgs/%s/invitations, revoked by hand)",
			lastSweepErr.Error(), orgID)
	}
	switch {
	case sweepErr != nil && lastErr != nil:
		httpError(w, http.StatusInternalServerError,
			"the org membership of %s was removed but re-sweeping their team access failed: %s, and re-deleting their org membership failed too: %s — they may still hold a team grant in this org, and an org membership written while the removal was in flight may stand, so they would be an org member again%s; re-issue the removal",
			memberID, sweepErr.Error(), lastErr.Error(), andThen(lastSweepResidue))
	case sweepErr != nil:
		httpError(w, http.StatusInternalServerError,
			"the org membership of %s was removed but re-sweeping their team access failed: %s — they may still hold a team grant in this org%s; re-issue the removal",
			memberID, sweepErr.Error(), andThen(lastSweepResidue))
	case lastErr != nil && lastSweepErr != nil:
		// Both tail writes failed: the grants ARE revoked (the second pass
		// went through) but the invitations are not, so this exit carries
		// neither the grant residual nor the promise that the invitations
		// were taken.
		httpError(w, http.StatusInternalServerError,
			"the team grants of %s in org %s are revoked but re-deleting their org membership failed: %s — an org membership written while the removal was in flight may stand, and they would be an org member again, and %s; re-issue the removal",
			memberID, orgID, lastErr.Error(), lastSweepResidue)
	case lastErr != nil:
		httpError(w, http.StatusInternalServerError,
			"the team grants of %s in org %s are revoked, with "+revokedMintedInvitations+", but re-deleting their org membership failed: %s — an org membership written while the removal was in flight may stand, and they would be an org member again; re-issue the removal",
			memberID, orgID, lastErr.Error())
	case lastSweepErr != nil:
		httpError(w, http.StatusInternalServerError,
			"the org membership of %s was removed and their team grants in org %s are revoked, but %s; re-issue the removal",
			memberID, orgID, lastSweepResidue)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// andThen appends a clause to a body that already carries one, and nothing
// at all when there is none.
func andThen(clause string) string {
	if clause == "" {
		return ""
	}
	return ", and " + clause
}

// revokeOrgTeamAccess deletes everything that gives userID access to a team
// of orgID and everything they left pending there — the grants they hold, the
// not-yet-accepted invitations they MINTED above the standing the removal
// leaves them, and, where the account's own address can be read, the ones
// addressed to them — and records one audit line per row actually revoked. It re-reads the org's teams, the user's
// grants and each team's invitations on each call, so a
// team, a grant or an invitation created since a previous call is covered. Re-running it revokes nothing twice:
// DeleteMembership answers nil when no row matched, and an invitation some
// other caller revoked in between is skipped on its ErrNotFound.
//
// A pending invitation is access the same way a grant is: accepting one
// writes the team grant AND, for any role on the org ladder, the org
// membership that was just deleted (pkg/auth grantOrgMembershipForTeam), so
// an off-boarding that swept only the grants is undone by one request, with
// no race to win. An invitation the removed member minted is the same access
// one step removed: the accept reads nothing about its creator, so one
// carrying an org role would hand out a rank the org's roster no longer
// holds them at. A removal is the standing dropping to NONE, so what they
// minted is measured by the demotion's own predicate against that standing
// (orgMirrorAbove, orgStandingAfterRemoval): every role on the ladder is
// above it and goes, and the one mirrored up to NOTHING (config_editor,
// ADR-078) outranks no standing and stands, as it does on a demotion —
// unless it is addressed to the member themselves, which the other half
// takes whatever its role, since the accept would seat the off-boarded
// member back in a team of the org.
//
// What the mint's own confirmation does with the row that lands after this
// listing is the same predicate with one precondition added, not the same
// selection (confirmOrgStandingAfterMint): a removal takes the
// self-addressed row whatever grant its creator holds, because it revokes
// that grant in the same pass, while the confirmation keeps it from a minter
// who still HOLDS a grant in the team — the orphan-grant holder minting for
// themselves with no removal in progress, a state the console reports
// (orphan_grant, off_ladder) rather than refuses. So the two sides differ on
// exactly one state, by design, and the operator-visible consequence is that
// re-issuing the removal takes a row a mint kept.
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
// That order is also why the invitation half runs ONCE MORE, alone, as the
// removal's last write: inside a pass it precedes the grant revoke, so a row
// minted between the two is seen by no sweep and kept by a confirmation
// reading a grant that same pass is about to take (handleRemoveOrgMember).
//
// Not-yet-accepted is deliberately wider than the accept paths' "still
// acceptable": matching their clock here would be a second predicate to keep
// in step with theirs, and taking a row that was already dead costs nothing.
// An ACCEPTED invitation stays — it is the trail of how the member joined.
//
// The store work runs on ctx, which the caller chooses: a sweep that must
// outlive the request passes one detached from it. r names the audit actor.
func (s *Server) revokeOrgTeamAccess(ctx context.Context, r *http.Request, orgID, userID string) error {
	if err := s.revokeOrgTeamInvitations(ctx, r, orgID, userID); err != nil {
		return err
	}
	teams, err := s.authStore().ListTeamsByOrg(ctx, orgID)
	if err != nil {
		return fmt.Errorf("list org teams: %w", err)
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

// revokeOrgTeamInvitations deletes the not-yet-accepted invitations in the
// teams of orgID that the standing a removal leaves userID with takes from
// them — the ones they MINTED whose role carries an org role and, where the
// account's own address can be read, the ones addressed to them
// (standingTakesInvitation against orgStandingAfterRemoval) — with one audit
// line per row actually revoked.
//
// It is the invitation half of revokeOrgTeamAccess, callable ALONE because
// the removal runs it once more after every other write it makes: inside a
// pass the half precedes the grant revoke, which is the premise the mint's
// own confirmation reads, so only a selection with no write after it can say
// that a removal which answered 204 left no invitation row its rule takes on
// a premise THIS removal withdrew (handleRemoveOrgMember). A premise another
// writer puts back after that selection is that writer's to withdraw, and
// the rows kept on a grant go with it
// (revokeInvitationsKeptOnTeamGrant).
//
// It re-reads the org's teams and each team's invitations on every call, so a
// team or an invitation created since a previous call is covered, and it
// takes nothing twice: a row another caller revoked in between is skipped on
// its ErrNotFound (revokePendingInvitations).
//
// The store work runs on ctx, which the caller chooses: a sweep that must
// outlive the request passes one detached from it. r names the audit actor.
func (s *Server) revokeOrgTeamInvitations(ctx context.Context, r *http.Request, orgID, userID string) error {
	teams, err := s.authStore().ListTeamsByOrg(ctx, orgID)
	if err != nil {
		return fmt.Errorf("list org teams: %w", err)
	}
	// An invitation names an ADDRESS, not an account, so the sweep needs the
	// one this account answers at (readOwnInvitationAddress). A row that may
	// exist and cannot be read stops the cascade: an off-boarding that
	// skipped the invitations on it would report a cascade it did not finish.
	//
	// An account that definitively has NO row is the other case, and it is
	// not an error here: there is no address an invitation could be addressed
	// to, and no accept could turn one into access for THIS id — a
	// registration mints a new account id (pkg/auth registerWithInvitation)
	// and the existing-user accept logs in as this account. The caller names
	// an id the request path gave it, which this endpoint neither requires to
	// be a member nor refuses when unknown, so a fatal read there would leave
	// the grants AND the membership row it exists to take. The ADDRESSED-TO
	// half alone is skipped, logged, and the rest of the cascade runs — the
	// invitations this account MINTED are selected by its id, which the path
	// gave, and are swept either way.
	//
	// The line reports what this read found, never what the removal will
	// do: the pass it opens can still fail with everything standing, and the
	// caller's own answer is what says which state it left.
	addr, addressKnown, err := s.readOwnInvitationAddress(ctx, userID)
	if err != nil {
		return err
	}
	if !addressKnown && s.logger != nil {
		s.logger.Warn("org %s: %s has no account row, so no address their pending invitations could be addressed to — this sweep skips that half (the invitations they MINTED are selected by their id and swept), and any invitation addressed to them stands, listed by GET /api/orgs/%s/invitations",
			orgID, userID, orgID)
	}
	return s.revokePendingInvitations(ctx, requestActor(r), teams, "org_member_removed", func(inv identity.Invitation) bool {
		return standingTakesInvitation(userID, orgStandingAfterRemoval, addr, addressKnown, inv)
	})
}

// revokeInvitationsAboveOrgStanding deletes the pending invitations userID
// minted in the teams of orgID that the cap that would bind their minting NOW
// refuses: the role mirrored up to an org role above standing — the rank they
// hold there now — and, where that standing does not carry a team by itself
// (below admin), the role above their own grant in each team (K.2, the same
// predicate requireRankToInvite applies at the mint). It is what keeps a cap
// read at the MINT from being outlived: the accept reads nothing about an
// invitation's creator, so one minted at a rank its creator has since lost
// would still hand that rank out. It covers the rows its listing reaches; a
// mint whose row lands after that listing is covered by the mint's own
// confirmation (confirmOrgStandingAfterMint), which is the other half of the
// pair.
//
// It reconciles against the standing it is given, not against the one it
// replaced, so it is idempotent: issued again on a row that already carries
// that role, it revokes whatever a previous call could not.
//
// The grant is read once per team, and only where the standing written is
// below admin — the one state where the cap measures it at all. A grant that
// cannot be READ fails the sweep: a selection that cannot be made is not one
// that found nothing, and the caller answers 500 naming the act to re-issue.
func (s *Server) revokeInvitationsAboveOrgStanding(ctx context.Context, r *http.Request, orgID, userID string, standing identity.OrgRole) error {
	teams, err := s.authStore().ListTeamsByOrg(ctx, orgID)
	if err != nil {
		return fmt.Errorf("list org teams: %w", err)
	}
	actor := requestActor(r)
	// A demotion leaves a membership ROW, whose absence is what the
	// addressed-to half of standingTakesInvitation fires at, so this sweep
	// needs no address: the invitations addressed to a member who keeps a row
	// stand, where a removal takes them.
	for _, t := range teams {
		grant := identity.Role("")
		if !standing.AtLeast(identity.OrgRoleAdmin) {
			g, _, rerr := s.readTeamGrantRole(ctx, userID, t.ID)
			if rerr != nil {
				return fmt.Errorf("read the grant of %s in team %s: %w", userID, t.ID, rerr)
			}
			grant = g
		}
		if err := s.revokePendingInvitations(ctx, actor, []identity.Team{{ID: t.ID}}, "org_member_demoted", func(inv identity.Invitation) bool {
			return teamRankTakesInvitation(userID, true, standing, grant, inv)
		}); err != nil {
			return err
		}
	}
	return nil
}

// orgStandingAfterRemoval is the standing a removal leaves its target: no
// membership row at all, which ranks below every rung
// (identity.OrgRole.AtLeast). It is what makes the removal the demotion's
// own rule rather than a second one — the invitations the member minted are
// measured by orgMirrorAbove either way — and it is the standing that is
// GONE rather than merely lowered, the one the addressed-to half of
// standingTakesInvitation fires at.
const orgStandingAfterRemoval identity.OrgRole = ""

// standingTakesInvitation reports whether the org standing a write LEAVES
// member ownID with takes pending invitation inv from them. It is the one
// selection every write that lowers a standing makes, in two halves:
//
//   - the MINTED half — an invitation they minted whose role is mirrored up
//     to an org role above that standing (orgMirrorAbove) would hand out a
//     rank the roster no longer holds them at, so it goes; the role mirrored
//     up to NOTHING (config_editor, ADR-078) is above no standing and stands,
//     handed over rather than withdrawn;
//   - the ADDRESSED-TO half — once the standing is GONE, which is the
//     membership row's ABSENCE and not a rank: the half fires on the
//     orgStandingAfterRemoval sentinel its callers pass from a read that
//     found no row (a removal, never a demotion), so a rung this build does
//     not rank — one added to identity.OrgRole before OrgRole.rank — is a
//     member who still HOLDS a row and keeps what is addressed to them. An
//     invitation addressed to the address their own account answers at
//     (identity.Invitation.AddressedTo, read as both accept paths read it)
//     would seat them back in a team of the org they just left, so it goes
//     whatever its role — this half measures no rank. addrKnown is false
//     where no address could be read at all, an account with no row having
//     none an invitation could be addressed to, and the half then selects
//     nothing, and whether an address is in play at all is its caller's to
//     establish: the cascade reads the removed member's own, since it
//     revokes their grants in the same pass, and the mint's confirmation
//     reads one only where the minter's grant in the invitation's team is
//     gone too (standingAfterMintTakesInvitation).
//
// Both sweeps select with it over the rows their listing reached
// (revokeOrgTeamAccess, revokeInvitationsAboveOrgStanding) and the mint's
// confirmation answers it for the one row that landed after them
// (confirmOrgStandingAfterMint). It is one predicate, which is not the same
// as one selection: each caller establishes its own arguments, and the
// confirmation adds a precondition of its own before it asks the
// addressed-to half at all, so on the one state where they differ — a minter
// who still holds a grant in the team — the removal takes the row and the
// mint keeps it, by design and with no race
// (standingAfterMintTakesInvitation).
func standingTakesInvitation(ownID string, standing identity.OrgRole, addr string, addrKnown bool, inv identity.Invitation) bool {
	return (inv.InvitedBy == ownID && orgMirrorAbove(standing, inv.Role)) ||
		standingGoneTakesAddressedTo(standing, addr, addrKnown, inv)
}

// standingGoneTakesAddressedTo is the ADDRESSED-TO half of
// standingTakesInvitation, named so that a caller selecting on that half
// ALONE asks it rather than copying its terms: a write that withdraws the
// premise a row was kept on measures no rank at all
// (revokeInvitationsKeptOnTeamGrant). The gate is the standing being GONE,
// which its caller establishes from a read that found no membership row.
func standingGoneTakesAddressedTo(standing identity.OrgRole, addr string, addrKnown bool, inv identity.Invitation) bool {
	return standing == orgStandingAfterRemoval && addrKnown && inv.AddressedTo(addr)
}

// teamRankTakesInvitation reports whether the team authority a write LEAVES
// its target with refuses, at the mint, a pending invitation inv they minted
// — the question requireRankToInvite answers for a mint not yet made, asked
// here of one already standing, from the rows the write just left. It is the
// team twin of standingTakesInvitation, the one selection every write that
// LOWERS a team authority makes, and its shape is requireRankToInvite's own:
//
//   - K.1 — where the team has a parent org, the role's org MIRROR against
//     the creator's standing there: a mirror above it hands out a rank the
//     roster no longer holds them at. A team with no parent org has no
//     standing for this half to measure, as the mint has none to read;
//   - K.2 — the minted TEAM role against the creator's grant, where their
//     authority over the team IS that grant: no parent org, or a standing
//     below admin. An admin or owner standing carries the team by itself, as
//     it carried the mint, so a grant under it measures nothing — which is
//     also why a write that lowers a GRANT alone reconciles nothing for a
//     creator whose standing is that high: their authority did not drop.
//
// A creator with no org row reads as a standing of none, the same sentinel a
// removal leaves — the cap would refuse their mint now, and the sweep takes
// what it refused. A role mirrored up to NOTHING (config_editor, ADR-078) is
// measured by NEITHER half: the caps never measured it, no cap measures it
// now, and the row stands under every lowering.
//
// grant is the role the write left on the creator's row — the value written,
// or "" where the write took the grant away — and "" measures below every
// rung, as a grant that is not there does at the mint
// (readTeamGrantRole). It is the TEAM half of the same sweep the org writes
// run (revokeInvitationsAboveOrgStanding), which measures this predicate
// against the standing it was given.
func teamRankTakesInvitation(ownID string, hasOrg bool, standing identity.OrgRole, grant identity.Role, inv identity.Invitation) bool {
	if inv.InvitedBy != ownID {
		return false
	}
	want := identity.OrgRoleForTeamRole(inv.Role)
	if want == "" {
		return false
	}
	if hasOrg && !standing.AtLeast(want) {
		return true
	}
	if hasOrg && standing.AtLeast(identity.OrgRoleAdmin) {
		return false
	}
	return !grant.AtLeast(inv.Role)
}

// revokeInvitationsKeptOnTeamGrant deletes the pending invitations of teamID
// that the mint's own confirmation kept on userID's GRANT in it: the ones
// they addressed to their own address while holding no org membership row in
// the team's org (standingAfterMintTakesInvitation). Whoever withdraws that
// premise owes this sweep — DELETE /api/teams/{id}/members/{user_id}, the
// grant compensation and the GitHub SSO reconciliation revoke a team grant
// and leave the team standing with no removal in flight — since the rows
// kept on it carry no org RANK for a standing to move and no other write
// measures them again: what is left is a re-entry ticket into the team the
// grant was just taken from.
//
// It takes only what that grant was the premise of. A row addressed to
// SOMEBODY ELSE is kept whatever grant its creator holds — a capability
// handed over, which no standing of theirs outranks — and the rows of a
// member whose org membership row ANSWERED the caller's read rest on that
// ROW, which this write does not touch; both are the caller's to establish
// before it calls (the row's absence) or stand here (the address). A row at
// a role carrying an org MIRROR belongs to the other half, measured against
// its creator's org standing by the writes that move it
// (standingTakesInvitation).
//
// That absence is read ONCE, by the caller, before this selection: a
// membership row written back in between is not seen, so the rows resting
// on it are taken under reason too. What such a window costs is a
// capability LOST and never one handed out — nothing is granted and no org
// rank is written — each row taken is in the team's audit trail, and
// minting the invitation again is the repair.
//
// An account that definitively has no row has no address an invitation could
// be addressed to, so the selection takes nothing and says so; any other
// failure of that read is returned, a half that cannot be applied being its
// caller's to answer for. reason is the audit cause of each row taken,
// naming the write that withdrew the premise. One team is swept — the one
// whose grant went — through the sweep body the removal and the demotion
// share, which reads each team by id (revokePendingInvitations), so the
// ErrNotFound skip and the audit line cannot drift between them.
//
// The store work runs on ctx, which the caller chooses: a sweep that must
// outlive the request passes one detached from it. actor names who each audit
// line is attributed to — the request a write serves, or the subsystem of a
// write that has none (auditActor).
func (s *Server) revokeInvitationsKeptOnTeamGrant(ctx context.Context, actor auditActor, teamID, userID, reason string) error {
	addr, addrKnown, err := s.readOwnInvitationAddress(ctx, userID)
	if err != nil {
		return err
	}
	if !addrKnown {
		if s.logger != nil {
			s.logger.Warn("team %s: %s has no account row, so no address a pending invitation could be addressed to — the grant that just went was the premise of none",
				teamID, userID)
		}
		return nil
	}
	return s.revokePendingInvitations(ctx, actor, []identity.Team{{ID: teamID}}, reason, func(inv identity.Invitation) bool {
		return inv.InvitedBy == userID &&
			standingGoneTakesAddressedTo(orgStandingAfterRemoval, addr, addrKnown, inv)
	})
}

// takeTeamRankInvitations sweeps ONE team's pending invitations for the rows
// the cap that would bind their creator NOW refuses (teamRankTakesInvitation)
// — what a write that just lowered that creator's team authority owes. It is
// the team twin of the org writes' sweep (revokeInvitationsAboveOrgStanding),
// over the one team the write acted in: the creator's standing is read from
// the row, their grant is the value the caller carries from the write (the
// role written, or "" where the write took the grant away).
//
// A team with no parent org measures K.2 alone, as the mint does there; a
// team that no longer exists takes nothing, a row of it resolving to no
// accept. The creator's standing being UNREADABLE fails the sweep — a
// selection that cannot be made is not one that found nothing — and each
// caller reports it in the words it owes.
//
// The store work runs on ctx, which the caller chooses: a sweep that must
// outlive the request passes one detached from it. actor names who each audit
// line is attributed to; reason is the cause of each row taken, the same
// creator_team_rank_lowered on every lowering write.
func (s *Server) takeTeamRankInvitations(ctx context.Context, actor auditActor, teamID, userID string, grant identity.Role, reason string) error {
	t, err := s.authStore().GetTeam(ctx, teamID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("resolve the team: %w", err)
	}
	var standing identity.OrgRole
	if t.OrgID != "" {
		have, _, err := s.readOwnOrgStanding(ctx, userID, t.OrgID)
		if err != nil {
			return fmt.Errorf("read the org standing of %s in %s: %w", userID, t.OrgID, err)
		}
		standing = have
	}
	return s.revokePendingInvitations(ctx, actor, []identity.Team{{ID: teamID}}, reason, func(inv identity.Invitation) bool {
		return teamRankTakesInvitation(userID, t.OrgID != "", standing, grant, inv)
	})
}

// reconcileTeamRankInvitations answers the request and returns false when the
// sweep a team-authority-lowering write owes could not run: it is what keeps
// the caps read at the MINT (requireRankToInvite) from being outlived by a
// row minted inside their own window, the same way the org writes reconcile
// (reconcileMintedInvitations). The refusal names what stands, the listing
// that finds it and the act to re-issue — re-issuing the write measures the
// same cap and finishes the sweep.
//
// grant is the role the write left on the member's row (the role written, or
// "" where it took the grant away): the sweep decides from the rows the write
// just wrote, not from a re-read a racer could move. act names what the
// caller did ("role change", "placement", "removal") for the body that asks
// for it again.
//
// It runs on a context DETACHED from the request, on its own budget, as the
// org writes' does: the authority is already lowered, so a mint racing in is
// capped by the new one, and a client that goes away here would otherwise
// leave exactly the invitations this sweep exists to revoke. It is the
// write's LAST invitation action, where the removal's ordering rules apply:
// a selection with no write of this request after it is what lets a 204 or a
// 200 say that no row the rule takes stands behind it.
func (s *Server) reconcileTeamRankInvitations(w http.ResponseWriter, r *http.Request, teamID, userID string, grant identity.Role, act string) bool {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
	defer cancel()
	err := s.takeTeamRankInvitations(cctx, requestActor(r), teamID, userID, grant, "creator_team_rank_lowered")
	if err == nil {
		return true
	}
	if s.logger != nil {
		s.logger.Error("team %s: the team role of %s is now %s but revoking the pending invitations they minted above what that covers failed: %v — such an invitation still carries a rank its creator no longer holds; re-issue the %s",
			teamID, userID, teamGrantName(grant), err, act)
	}
	httpError(w, http.StatusInternalServerError,
		"the team role of %s in team %s is now %s but revoking the pending invitations they minted above what that covers failed: %s — such an invitation still carries a rank its creator no longer holds (listed by GET /api/teams/%s/invitations, revoked by hand); re-issue the %s to finish it",
		userID, teamID, teamGrantName(grant), err.Error(), teamID, act)
	return false
}

// teamGrantSweepFailure names the read the sweep after a revoked grant could
// not make, in the words every caller of that sweep owes: which read failed
// decides what may stand, and a body blaming the standing for a team read
// would send an operator to a row that was read fine.
type teamGrantSweepFailure struct {
	// clause names the read in an answer addressed to the caller ("reading
	// their org membership in o1 failed").
	clause string
	// residue names what may stand because that read could not be made.
	residue string
	// logClause and logResidue are the same two in the log line, which says
	// who they are about rather than addressing them.
	logClause, logResidue string
	// err is the failure itself.
	err error
}

// takeInvitationsKeptOnRevokedTeamGrant establishes the premise of the sweep
// a write that just took userID's grant in teamID owes — the team's parent
// org, and that the holder has NO membership row in it — and takes the rows
// kept on that grant (revokeInvitationsKeptOnTeamGrant). It is the ONE
// selection the three writes that owe it share: the HTTP writes through
// sweepInvitationsKeptOnTeamGrant, and the auth service's own through the
// sweep the server registers on it (wireTeamGrantSweep, auth.TeamGrantSweep).
//
// A team with no parent org is exempt, as the mint's confirmation is: there
// was no org standing to be gone, so no row was ever kept on this grant. A
// team that definitively no longer exists is too — a row of a team no accept
// can resolve (pkg/auth grantOrgMembershipForTeam) buys nothing.
//
// The premise is read here, once, and the rows are selected after it: a
// membership row written back in between — a placement, an accept, a role
// change — is not seen, and the invitations it keeps are swept. So an answer
// of nil says no row stands on the grant for the holder as THIS read found
// them, not as another writer left them. That window costs an invitation
// that stops working and nothing else: no rank is written and nothing is
// granted, the rows taken are in the team's audit trail under reason, and
// minting the invitation again is the repair.
//
// actor names who each audit line is attributed to. It answers nil when the
// sweep ran or was exempt, and the read that failed otherwise — a selection
// that could not be made is not one that found nothing, which each caller
// reports in the words it owes.
func (s *Server) takeInvitationsKeptOnRevokedTeamGrant(ctx context.Context, actor auditActor, teamID, userID, reason string) *teamGrantSweepFailure {
	t, err := s.authStore().GetTeam(ctx, teamID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return nil
	case err != nil:
		return &teamGrantSweepFailure{
			clause:     "resolving this team's organization failed",
			residue:    "a pending invitation they addressed to their own address here would stand on a grant that is gone",
			logClause:  "resolving the team's org failed",
			logResidue: "a pending invitation they addressed to themselves here may stand on a grant that is gone",
			err:        err,
		}
	case t.OrgID == "":
		return nil
	}
	_, hasRow, err := s.readOwnOrgStanding(ctx, userID, t.OrgID)
	if err != nil {
		return &teamGrantSweepFailure{
			clause:     fmt.Sprintf("reading their org membership in %s failed", t.OrgID),
			residue:    "a pending invitation they addressed to their own address here would stand on a grant that is gone",
			logClause:  fmt.Sprintf("reading their org membership in %s failed", t.OrgID),
			logResidue: "a pending invitation they addressed to themselves here may stand on a grant that is gone",
			err:        err,
		}
	}
	// The row is THERE as THIS read answers: what they minted for themselves
	// rests on that row, which this write does not touch, and the writes that
	// lower it sweep on their own (reconcileMintedInvitations,
	// revokeOrgTeamInvitations). A row written back after this read is read as
	// absent, and what rests on it goes with the sweep below.
	if hasRow {
		return nil
	}
	if err := s.revokeInvitationsKeptOnTeamGrant(ctx, actor, teamID, userID, reason); err != nil {
		return &teamGrantSweepFailure{
			clause:     "sweeping the pending invitations they addressed to their own address here failed",
			residue:    "such a row stands on a grant that is gone and accepting it would seat them back in this team",
			logClause:  "sweeping the pending invitations kept on it failed",
			logResidue: "such a row is a re-entry ticket into this team",
			err:        err,
		}
	}
	return nil
}

// sweepInvitationsKeptOnTeamGrant answers the request and returns false when
// a write that just took userID's grant in teamID could not sweep the rows
// the mint's confirmation kept on it
// (takeInvitationsKeptOnRevokedTeamGrant). The 500 names the read that
// failed, the row that may stand, the listing this route's caller can read
// and the act that finishes it: a selection that could not be made is not
// one that found nothing.
//
// It runs on a context detached from the request, on its own budget: the
// grant is already gone, so a client that goes away here would otherwise
// leave exactly the row this sweep exists to take.
func (s *Server) sweepInvitationsKeptOnTeamGrant(w http.ResponseWriter, r *http.Request, teamID, userID, reason string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), grantCompensationTimeout)
	defer cancel()
	f := s.takeInvitationsKeptOnRevokedTeamGrant(ctx, requestActor(r), teamID, userID, reason)
	if f == nil {
		return true
	}
	if s.logger != nil {
		s.logger.Error("team %s: the grant of %s is revoked but %s: %v — %s",
			teamID, userID, f.logClause, f.err, f.logResidue)
	}
	httpError(w, http.StatusInternalServerError,
		"the team grant of %s is revoked but %s: %v — %s (listed by GET /api/teams/%s/invitations, revoked by hand); re-issue the removal to finish it",
		userID, f.clause, f.err, f.residue, teamID)
	return false
}

// orgMirrorAbove reports whether a team role's org mirror
// (identity.OrgRoleForTeamRole) is above standing — the question an
// invitation's role is measured by once its creator's standing moves. A role
// mirrored up to NOTHING (config_editor, rank 0 — ADR-078 decision 4) is
// above no standing and is excluded explicitly: AtLeast answers false for an
// empty want by construction, which would read as "above" and revoke the one
// invitation no standing can outrank.
func orgMirrorAbove(standing identity.OrgRole, role identity.Role) bool {
	want := identity.OrgRoleForTeamRole(role)
	return want != "" && !standing.AtLeast(want)
}

// revokePendingInvitations deletes every not-yet-accepted invitation of teams
// that take selects, with one audit line per row actually revoked naming
// reason. A row another caller revoked between the listing and the delete is
// already in the state this sweep wants, and both stores say so definitively
// — where DeleteMembership cannot tell a grant it removed from one that was
// never there — so its ErrNotFound is skipped, with no audit line for a row
// this sweep did not take, and re-running it revokes nothing twice.
//
// Not-yet-accepted is deliberately wider than the accept paths' "still
// acceptable": matching their clock here would be a second predicate to keep
// in step with theirs, and taking a row that was already dead costs nothing.
// An ACCEPTED invitation stays — it is the trail of how a member joined.
//
// The caller passes the org's teams, which it has already read for its own
// reasons, and the ctx the store work runs on: a sweep that must outlive the
// request passes one detached from it. r names the audit actor.
func (s *Server) revokePendingInvitations(ctx context.Context, actor auditActor, teams []identity.Team, reason string, take func(identity.Invitation) bool) error {
	for _, t := range teams {
		invs, err := s.authStore().ListInvitationsByTeam(ctx, t.ID)
		if err != nil {
			return fmt.Errorf("list team %s invitations: %w", t.ID, err)
		}
		for _, inv := range invs {
			if inv.AcceptedAt != nil || !take(inv) {
				continue
			}
			if err := s.authStore().DeleteInvitation(ctx, inv.ID); err != nil {
				if errors.Is(err, identity.ErrNotFound) {
					continue
				}
				return fmt.Errorf("revoke team %s invitation: %w", t.ID, err)
			}
			s.auditTenantAs(actor, t.ID, "invitation.deleted", "invitation", inv.ID, map[string]any{"email": inv.Email, "reason": reason})
		}
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
	ok, grantCapped := s.requireRankToInvite(w, r, id, t.OrgID, teamID, role)
	if !ok {
		return
	}
	tok, inv, err := s.authSvc.CreateInvitation(r.Context(), teamID, req.Email, role, id.UserID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditOrg(r, orgID, "org_invitation.created", "invitation", inv.ID, map[string]any{"email": inv.Email, "role": string(inv.Role)})
	// Each cap read before the row landed, confirmed once it has: the same
	// pairs the team route owes (confirmOrgStandingAfterMint,
	// confirmTeamGrantAfterMint), before the token leaves by email or in the
	// response.
	if !s.confirmOrgStandingAfterMint(w, r, id, t.OrgID, inv) {
		return
	}
	if !s.confirmTeamGrantAfterMint(w, r, id, grantCapped, inv) {
		return
	}
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

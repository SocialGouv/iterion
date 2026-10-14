package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/mail"
)

// ---- Authenticated handlers ----

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	u, err := s.authStore().GetUser(r.Context(), id.UserID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "load user: %v", err)
		return
	}
	orgs, _ := s.buildOrgTree(r.Context(), u.ID)
	writeJSON(w, AuthMeResponse{
		User:          s.toUserView(u),
		Orgs:          orgs,
		ActiveOrg:     id.OrgID,
		ActiveOrgRole: string(id.OrgRole),
		ActiveTeam:    id.TeamID,
		ActiveRole:    string(id.Role),
	})
}

func (s *Server) handleSwitchTeam(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("team_id")
	if teamID == "" {
		httpError(w, http.StatusBadRequest, "team_id required")
		return
	}
	newID, access, exp, err := s.authSvc.SwitchTeam(r.Context(), id.UserID, teamID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.setAuthCookies(w, access, exp, "", time.Time{})
	s.writeIdentityResponse(w, r, newID, access, exp)
}

func (s *Server) handleSwitchOrg(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("org_id")
	if orgID == "" {
		httpError(w, http.StatusBadRequest, "org_id required")
		return
	}
	newID, access, exp, err := s.authSvc.SwitchOrg(r.Context(), id.UserID, orgID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.setAuthCookies(w, access, exp, "", time.Time{})
	s.writeIdentityResponse(w, r, newID, access, exp)
}

// ---- Team management ----

func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	memberships, err := s.authStore().ListMembershipsByUser(r.Context(), id.UserID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list memberships: %v", err)
		return
	}
	teamIDs := make([]string, 0, len(memberships))
	for _, m := range memberships {
		teamIDs = append(teamIDs, m.TeamID)
	}
	// One bulk fetch instead of a GetTeam per membership. A team absent
	// from the map is skipped exactly like the old per-row ErrNotFound;
	// an infra failure renders the same empty list it always did, but
	// is logged instead of vanishing.
	teamsByID, err := s.authStore().GetTeamsByIDs(r.Context(), teamIDs)
	if err != nil && s.logger != nil {
		s.logger.Warn("auth: bulk-load teams for user %s: %v", id.UserID, err)
	}
	views := make([]MembershipView, 0, len(memberships))
	for _, m := range memberships {
		t, ok := teamsByID[m.TeamID]
		if !ok {
			continue
		}
		views = append(views, MembershipView{
			TeamID:   t.ID,
			TeamName: t.Name,
			TeamSlug: t.Slug,
			Role:     string(m.Role),
			Personal: t.Personal,
		})
	}
	writeJSON(w, struct {
		Teams []MembershipView `json:"teams"`
	}{Teams: views})
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var req createTeamReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name required")
		return
	}
	// A team is created inside the caller's active org; org admins (or
	// super-admins) may add teams. The request may override the target
	// org explicitly (org console flows).
	orgID := req.OrgID
	if orgID == "" {
		orgID = id.OrgID
	}
	if orgID == "" {
		httpError(w, http.StatusBadRequest, "no active organization")
		return
	}
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	t, err := s.authSvc.CreateTeamFor(r.Context(), id.UserID, orgID, req.Name, req.Slug)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	writeJSON(w, t)
}

func (s *Server) handleListTeamMembers(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	ms, err := s.authStore().ListMembershipsByTeam(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list members: %v", err)
		return
	}
	type memberView struct {
		UserID string `json:"user_id"`
		Email  string `json:"email,omitempty"`
		Name   string `json:"name,omitempty"`
		Role   string `json:"role"`
	}
	userIDs := make([]string, 0, len(ms))
	for _, m := range ms {
		userIDs = append(userIDs, m.UserID)
	}
	// One bulk fetch instead of a GetUser per member. A missing user
	// yields the zero User — the row still renders with id + role, as
	// it did when the old per-row GetUser error was swallowed; an infra
	// failure is logged instead of vanishing.
	usersByID, err := s.authStore().GetUsersByIDs(r.Context(), userIDs)
	if err != nil && s.logger != nil {
		s.logger.Warn("auth: bulk-load members of team %s: %v", teamID, err)
	}
	out := make([]memberView, 0, len(ms))
	for _, m := range ms {
		u := usersByID[m.UserID]
		out = append(out, memberView{
			UserID: m.UserID,
			Email:  u.Email,
			Name:   u.Name,
			Role:   string(m.Role),
		})
	}
	writeJSON(w, struct {
		Members []memberView `json:"members"`
	}{Members: out})
}

func (s *Server) handleCreateInvitation(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req createInvitationReq
	if !decodeJSON(w, r, &req) {
		return
	}
	role := identity.Role(req.Role)
	if !role.Valid() {
		httpError(w, http.StatusBadRequest, "invalid role")
		return
	}
	// A mint is the one team-scoped act that hands out ORG standing, since
	// accepting the invitation mirrors its role up to an org membership, so
	// it is capped by the minter's own: the team names the org that cap is
	// read in. A team that cannot be read is a cap that cannot be applied,
	// which refuses the mint rather than minting uncapped.
	t, err := s.authStore().GetTeam(r.Context(), teamID)
	if errors.Is(err, identity.ErrNotFound) {
		// The words the service answers on its own read of the team, so a
		// team that is not there reads the same from either side.
		httpError(w, http.StatusNotFound, "%s", auth.ErrTeamNotFound.Error())
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "read the team: %v", err)
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
	s.auditTenant(r, teamID, "invitation.created", "invitation", inv.ID, map[string]any{"email": inv.Email, "role": string(inv.Role)})
	// When a real mailer is wired, deliver the invitation by email too
	// (detached — a relay blip must not fail the create). The in-band
	// token below stays: CLI/SDK flows and operators without SMTP
	// copy it manually.
	if s.authSvc.EmailEnabled() {
		team, terr := s.authStore().GetTeam(r.Context(), teamID)
		teamName := teamID
		if terr == nil {
			teamName = team.Name
		}
		msg := mail.RenderInvitation(inv.Email, mail.InviteData{
			TeamName:  teamName,
			Role:      string(inv.Role),
			AcceptURL: s.authSvc.PublicURL() + "/invitations/accept?token=" + tok,
			InvitedBy: id.Email,
		})
		s.goSafe("invitation-email", func() {
			bg, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := s.authSvc.Mailer().Send(bg, msg); err != nil && s.logger != nil {
				s.logger.Warn("auth: invitation email to %s: %v", msg.To, err)
			}
		})
	}
	// Return both the persistent ID and the plaintext token so the
	// admin can copy/email it. The plaintext is never recoverable
	// after this response.
	writeJSON(w, struct {
		ID        string    `json:"id"`
		Token     string    `json:"token"`
		Email     string    `json:"email"`
		Role      string    `json:"role"`
		ExpiresAt time.Time `json:"expires_at"`
	}{ID: inv.ID, Token: tok, Email: inv.Email, Role: string(inv.Role), ExpiresAt: inv.ExpiresAt})
}

func (s *Server) handleListInvitations(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	invs, err := s.authStore().ListInvitationsByTeam(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list invitations: %v", err)
		return
	}
	writeJSON(w, struct {
		Invitations []identity.Invitation `json:"invitations"`
	}{Invitations: invs})
}

func (s *Server) handleDeleteInvitation(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	inviteID := r.PathValue("invite_id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	inv, err := s.authStore().GetInvitation(r.Context(), inviteID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	if inv.TeamID != teamID {
		httpError(w, http.StatusNotFound, "invitation not in team")
		return
	}
	if err := s.authStore().DeleteInvitation(r.Context(), inviteID); err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "invitation.deleted", "invitation", inviteID, map[string]any{"email": inv.Email})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpdateMember(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	memberID := r.PathValue("user_id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req updateMemberReq
	if !decodeJSON(w, r, &req) {
		return
	}
	role := identity.Role(req.Role)
	if !role.Valid() {
		httpError(w, http.StatusBadRequest, "invalid role")
		return
	}
	mb, err := s.authStore().GetMembership(r.Context(), memberID, teamID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	// A role change reads the grant and writes it back, so it WRITES a grant
	// row like the placement does — an off-boarding that runs inside that
	// window finds nothing to sweep and the write-back re-creates a grant
	// with no org membership behind it. The team's org is what both guards
	// read, which is why a role change loads the team.
	t, err := s.authStore().GetTeam(r.Context(), teamID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	// Both guards read the boundary, which is skipped only when NEITHER the
	// role being written nor the one the grant already carries is mirrored up
	// to an org membership (grantNeedsOrgBoundary): the invitation path
	// writes exactly that off-ladder grant with no org membership, so a
	// re-role that both starts and stays off the ladder has nothing to
	// confirm. Reading the role being written alone would let the holder of
	// an orphan LADDER grant — who passes canManageTeam on that grant alone —
	// re-role THEMSELVES off the ladder and keep it in one request. The same
	// holder reaches that grant state in two — canManageTeam authorizes
	// minting an invitation to their own address, and accepting it off the
	// ladder writes no boundary — which is why `orphan_grant` reports the
	// missing boundary itself rather than a verdict on the role
	// (admin_users_routes.go). What that pair of requests can carry is capped
	// at the minter's own org standing (requireOrgStandingToInvite), since
	// pkg/auth mirrors an accepted ladder invitation up to an org membership
	// (grantOrgMembershipForTeam): the same holder mints only a role mirrored
	// up to NOTHING, which is the off-ladder grant this guard reads.
	// Climbing ONTO the ladder
	// needs the boundary, and so does placing any grant on a non-member
	// (handlePutTeamMember) — which is what keeps a team admin from pulling a
	// stranger account in.
	boundary := grantNeedsOrgBoundary(role) || grantNeedsOrgBoundary(mb.Role)
	if boundary && !s.requireOrgMembershipForGrant(w, r, t.OrgID, memberID) {
		return
	}
	mb.Role = role
	if err := s.authStore().UpsertMembership(r.Context(), mb); err != nil {
		// A write that reports an error may have LANDED: the compensation
		// owes this path too, and answers before the write error does.
		if boundary && !s.confirmOrgMembershipForGrant(w, r, t.OrgID, teamID, memberID, "role change", grantWriteFailed) {
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if boundary && !s.confirmOrgMembershipForGrant(w, r, t.OrgID, teamID, memberID, "role change", grantWriteAcked) {
		return
	}
	s.auditTenant(r, teamID, "member.role_changed", "member", memberID, map[string]any{"role": string(role)})
	writeJSON(w, mb)
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	memberID := r.PathValue("user_id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	if err := s.authStore().DeleteMembership(r.Context(), memberID, teamID); err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "member.removed", "member", memberID, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- Invitations (anonymous lookup + post-login accept) ----

func (s *Server) handleInvitationLookup(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	if tok == "" {
		httpError(w, http.StatusBadRequest, "token required")
		return
	}
	inv, err := s.authStore().GetInvitationByTokenHash(r.Context(), auth.HashRefreshToken(tok))
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	if inv.AcceptedAt != nil {
		httpError(w, http.StatusConflict, "invitation already accepted")
		return
	}
	if !inv.ExpiresAt.IsZero() && time.Now().After(inv.ExpiresAt) {
		httpError(w, http.StatusGone, "invitation expired")
		return
	}
	t, err := s.authStore().GetTeam(r.Context(), inv.TeamID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	writeJSON(w, struct {
		Email    string `json:"email"`
		Role     string `json:"role"`
		TeamID   string `json:"team_id"`
		TeamName string `json:"team_name"`
	}{Email: inv.Email, Role: string(inv.Role), TeamID: t.ID, TeamName: t.Name})
}

func (s *Server) handleInvitationAcceptForLoggedIn(w http.ResponseWriter, r *http.Request) {
	// Authenticated path. The middleware does NOT auto-gate this
	// route (it's listed in isPublicPath so anonymous lookup works);
	// we re-check identity here.
	id, ok := auth.FromContext(r.Context())
	if !ok {
		// Try to extract from cookie/bearer manually since it's a
		// public route.
		bearer := s.extractBearer(r)
		if bearer == "" || s.signer == nil {
			httpError(w, http.StatusUnauthorized, "login required to accept")
			return
		}
		var err error
		id, err = s.signer.Verify(bearer)
		if err != nil {
			httpError(w, http.StatusUnauthorized, "token invalid")
			return
		}
	}
	var body struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Token == "" {
		httpError(w, http.StatusBadRequest, "token required")
		return
	}
	mb, err := s.authSvc.AcceptInvitationForExistingUser(r.Context(), id.UserID, body.Token)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditTenant(r, mb.TeamID, "invitation.accepted", "member", id.UserID, map[string]any{"role": string(mb.Role)})
	writeJSON(w, mb)
}

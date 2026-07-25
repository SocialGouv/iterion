package server

import (
	"context"
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
	s.mux.Handle("GET /api/orgs/{id}/members", s.requireAuth(http.HandlerFunc(s.handleListOrgMembers)))
	s.mux.Handle("PATCH /api/orgs/{id}/members/{user_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateOrgMember)))
	s.mux.Handle("DELETE /api/orgs/{id}/members/{user_id}", s.requireAuth(http.HandlerFunc(s.handleRemoveOrgMember)))
	s.mux.Handle("GET /api/orgs/{id}/invitations", s.requireAuth(http.HandlerFunc(s.handleListOrgInvitations)))
	s.mux.Handle("POST /api/orgs/{id}/invitations", s.requireAuth(http.HandlerFunc(s.handleCreateOrgInvitation)))
	s.mux.Handle("DELETE /api/orgs/{id}/invitations/{invite_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteOrgInvitation)))
	s.mux.Handle("GET /api/orgs/{id}/usage", s.requireAuth(http.HandlerFunc(s.handleOrgUsage)))
	s.mux.Handle("GET /api/orgs/{id}/teams", s.requireAuth(http.HandlerFunc(s.handleListOrgTeams)))
	s.mux.Handle("POST /api/orgs/{id}/teams", s.requireAuth(http.HandlerFunc(s.handleCreateOrgTeam)))
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
	// membership). Any failure aborts BEFORE the org membership is touched,
	// so the member stays intact and the admin retries — never a half-removed
	// member still holding team grants.
	teams, err := s.authStore().ListTeamsByOrg(r.Context(), orgID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list org teams: %v", err)
		return
	}
	for _, t := range teams {
		if err := s.authStore().DeleteMembership(r.Context(), memberID, t.ID); err != nil {
			httpError(w, http.StatusInternalServerError, "revoke team %s membership: %v", t.ID, err)
			return
		}
	}
	if err := s.authStore().DeleteOrgMembership(r.Context(), memberID, orgID); err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.auditOrg(r, orgID, "org_member.removed", "member", memberID, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListOrgInvitations(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "org admin or owner required")
		return
	}
	// An org invitation is a team invitation against a team in the org;
	// list the union across the org's teams.
	teams, _ := s.authStore().ListTeamsByOrg(r.Context(), orgID)
	out := make([]identity.Invitation, 0)
	for _, t := range teams {
		invs, _ := s.authStore().ListInvitationsByTeam(r.Context(), t.ID)
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
		teamID = s.firstTeamInOrg(r.Context(), orgID)
	}
	if teamID == "" {
		httpError(w, http.StatusBadRequest, "org has no team to invite into")
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

// firstTeamInOrg returns the org's "primary team": the first non-personal
// team, else any team. It is the **storage tenant** for org-level resources
// that stay team-keyed by design — SSO providers/domains and the team an
// org invitation joins — so the org REST surface never has to re-key those
// stores (ADR-048 "As built"). Returns "" when the org has no team.
func (s *Server) firstTeamInOrg(ctx context.Context, orgID string) string {
	teams, err := s.authStore().ListTeamsByOrg(ctx, orgID)
	if err != nil || len(teams) == 0 {
		return ""
	}
	for _, t := range teams {
		if !t.Personal {
			return t.ID
		}
	}
	return teams[0].ID
}

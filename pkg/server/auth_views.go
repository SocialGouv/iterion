package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// ---- Request / response shapes ----

type AuthMeResponse struct {
	User          UserView      `json:"user"`
	Orgs          []OrgTreeView `json:"orgs"`
	ActiveOrg     string        `json:"active_org_id,omitempty"`
	ActiveOrgRole string        `json:"active_org_role,omitempty"`
	ActiveTeam    string        `json:"active_team_id,omitempty"`
	ActiveRole    string        `json:"active_role,omitempty"`
	AccessToken   string        `json:"access_token,omitempty"`
	ExpiresAt     string        `json:"expires_at,omitempty"`
}

// OrgTreeView is one organization the user belongs to, with the teams
// inside it they can access. It is the shape /api/auth/me returns so
// the SPA can render an Org picker → Team picker without extra calls.
// Exported (with AuthMeResponse / MembershipView / UserView): pkg/cli
// decodes /api/auth/me with these exact types (RemoteMe aliases
// AuthMeResponse), so the CLI's decode target cannot drift from this
// wire.
type OrgTreeView struct {
	OrgID    string           `json:"org_id"`
	OrgName  string           `json:"org_name"`
	OrgSlug  string           `json:"org_slug"`
	OrgRole  string           `json:"org_role"`
	Personal bool             `json:"personal,omitempty"`
	Teams    []MembershipView `json:"teams"`
}

type UserView struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	Name         string `json:"name,omitempty"`
	Status       string `json:"status"`
	IsSuperAdmin bool   `json:"is_super_admin"`
	CreatedAt    string `json:"created_at,omitempty"`
}

type MembershipView struct {
	TeamID   string `json:"team_id"`
	TeamName string `json:"team_name"`
	TeamSlug string `json:"team_slug"`
	Role     string `json:"role"`
	Personal bool   `json:"personal,omitempty"`
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type changePasswordReq struct {
	Email           string `json:"email"`
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type registerReq struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	Name       string `json:"name,omitempty"`
	Invitation string `json:"invitation,omitempty"`
}

type createTeamReq struct {
	Name  string `json:"name"`
	Slug  string `json:"slug,omitempty"`
	OrgID string `json:"org_id,omitempty"`
}

type createInvitationReq struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

type updateMemberReq struct {
	Role string `json:"role"`
}

type adminUpdateUserReq struct {
	Status       *string `json:"status,omitempty"`
	IsSuperAdmin *bool   `json:"is_super_admin,omitempty"`
	Name         *string `json:"name,omitempty"`
}

// ---- Helpers ----

func (s *Server) toUserView(u identity.User) UserView {
	return UserView{
		ID:           u.ID,
		Email:        u.Email,
		Name:         u.Name,
		Status:       string(u.Status),
		IsSuperAdmin: u.IsSuperAdmin,
		CreatedAt:    u.CreatedAt.Format(time.RFC3339),
	}
}

// isBrowserClient reports whether the caller is a browser. We use it
// to suppress the access_token field from the JSON body: browsers
// receive the JWT via the HttpOnly cookie set by setAuthCookies, so
// echoing it in the body would defeat the HttpOnly protection and
// expose the token to any future XSS in the SPA. CLI/SDK clients
// can't read Set-Cookie reliably and still need the token in the body.
//
// Browsers send Origin on cross-origin fetches and Sec-Fetch-Site on
// every request (Chrome/Firefox/Safari since 2020). Treating their
// presence as the browser tell is conservative — false positives
// only force a CLI client to fall back to the cookie path, never the
// reverse.
func isBrowserClient(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("Sec-Fetch-Mode") != "" {
		return true
	}
	if r.Header.Get("Origin") != "" {
		return true
	}
	return false
}

func (s *Server) renderAuthResponse(w http.ResponseWriter, r *http.Request, res auth.LoginResult) {
	orgs, _ := s.buildOrgTree(r.Context(), res.User.ID)
	s.setAuthCookies(w, res.AccessToken, res.AccessExpires, res.RefreshToken, res.RefreshExpires)
	resp := AuthMeResponse{
		User:          s.toUserView(res.User),
		Orgs:          orgs,
		ActiveOrg:     res.ActiveOrgID,
		ActiveOrgRole: string(res.ActiveOrgRole),
		ActiveTeam:    res.ActiveTeamID,
		ActiveRole:    string(res.ActiveRole),
		ExpiresAt:     res.AccessExpires.Format(time.RFC3339),
	}
	if !isBrowserClient(r) {
		resp.AccessToken = res.AccessToken
	}
	writeJSON(w, resp)
}

// buildOrgTree assembles the org→teams view for a user: every org they
// belong to (via OrgMembership, plus any org reachable through a team
// grant), each carrying the teams in it they can access. Org admins see
// every team in their org; plain members see only their granted teams.
func (s *Server) buildOrgTree(ctx context.Context, userID string) ([]OrgTreeView, error) {
	st := s.authStore()
	if st == nil {
		return nil, nil
	}
	// Org membership is the source of truth for which orgs a user can see:
	// every team grant mirrors up to an org membership (signup, invite, SSO,
	// migration), so iterating org memberships covers every reachable org
	// without a GetTeam per team grant.
	orgMems, _ := st.ListOrgMembershipsByUser(ctx, userID)
	teamMems, _ := st.ListMembershipsByUser(ctx, userID)
	teamRole := make(map[string]identity.Role, len(teamMems))
	for _, m := range teamMems {
		teamRole[m.TeamID] = m.Role
	}
	orgIDs := make([]string, 0, len(orgMems))
	for _, om := range orgMems {
		orgIDs = append(orgIDs, om.OrgID)
	}
	// One bulk fetch instead of a GetOrg per membership. An org absent
	// from the map is skipped exactly like the old per-row ErrNotFound;
	// an infra failure renders the same empty tree it always did, but
	// is logged instead of vanishing.
	orgsByID, err := st.GetOrgsByIDs(ctx, orgIDs)
	if err != nil && s.logger != nil {
		s.logger.Warn("auth: bulk-load orgs for user %s: %v", userID, err)
	}
	out := make([]OrgTreeView, 0, len(orgMems))
	for _, om := range orgMems {
		org, ok := orgsByID[om.OrgID]
		if !ok {
			continue
		}
		orgRole := om.Role
		teams, _ := st.ListTeamsByOrg(ctx, om.OrgID)
		tv := make([]MembershipView, 0, len(teams))
		for _, t := range teams {
			role, granted := teamRole[t.ID]
			if !granted {
				// Org admins can see/manage every team in their org.
				if !orgRole.AtLeast(identity.OrgRoleAdmin) {
					continue
				}
				role = identity.RoleAdmin
			}
			tv = append(tv, MembershipView{
				TeamID:   t.ID,
				TeamName: t.Name,
				TeamSlug: t.Slug,
				Role:     string(role),
				Personal: t.Personal,
			})
		}
		out = append(out, OrgTreeView{
			OrgID:    org.ID,
			OrgName:  org.Name,
			OrgSlug:  org.Slug,
			OrgRole:  string(orgRole),
			Personal: org.Personal,
			Teams:    tv,
		})
	}
	return out, nil
}

// writeIdentityResponse renders the full auth response (user + org tree
// + active context) after a team/org switch, where we have a fresh
// Identity and access token but not a full LoginResult.
func (s *Server) writeIdentityResponse(w http.ResponseWriter, r *http.Request, id auth.Identity, access string, exp time.Time) {
	u, err := s.authStore().GetUser(r.Context(), id.UserID)
	if err != nil {
		u = identity.User{ID: id.UserID, Email: id.Email}
	}
	orgs, _ := s.buildOrgTree(r.Context(), id.UserID)
	resp := AuthMeResponse{
		User:          s.toUserView(u),
		Orgs:          orgs,
		ActiveOrg:     id.OrgID,
		ActiveOrgRole: string(id.OrgRole),
		ActiveTeam:    id.TeamID,
		ActiveRole:    string(id.Role),
		ExpiresAt:     exp.Format(time.RFC3339),
	}
	if access != "" && !isBrowserClient(r) {
		resp.AccessToken = access
	}
	writeJSON(w, resp)
}

// authStore returns the identity.Store used by the embedded auth
// service. We can't do this generically through the auth package's
// public API today, so we read it back via reflection-free helpers.
// This is a small layering hack: when auth.Service exposes Store()
// in a future revision, drop this.
func (s *Server) authStore() identity.Store {
	if s.authSvc == nil {
		return nil
	}
	return s.authSvc.Store()
}

func mapAuthErrorStatus(err error) int {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials),
		errors.Is(err, auth.ErrAccountDisabled),
		errors.Is(err, auth.ErrSessionRevoked),
		errors.Is(err, auth.ErrSessionExpired),
		errors.Is(err, auth.ErrTokenExpired),
		errors.Is(err, auth.ErrTokenInvalid),
		errors.Is(err, auth.ErrTokenRevoked):
		return http.StatusUnauthorized
	case errors.Is(err, auth.ErrSignupClosed),
		errors.Is(err, auth.ErrInvitationMismatch),
		errors.Is(err, auth.ErrPasswordWeak):
		return http.StatusBadRequest
	case errors.Is(err, auth.ErrLinkRequiresConsent):
		// 409 Conflict: an account exists with the same email but we
		// refuse to auto-link the new SSO identity. The UI should
		// prompt the user to log in with their password, then link
		// the SSO connection from settings.
		return http.StatusConflict
	case errors.Is(err, auth.ErrInvitationNotFound),
		errors.Is(err, auth.ErrTeamNotFound),
		errors.Is(err, identity.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, identity.ErrEmailAlreadyTaken),
		errors.Is(err, identity.ErrSlugAlreadyTaken),
		errors.Is(err, identity.ErrInvitationUsed):
		return http.StatusConflict
	case errors.Is(err, identity.ErrInvitationExpired):
		return http.StatusGone
	case errors.Is(err, auth.ErrNotAMember),
		errors.Is(err, auth.ErrPasswordChangeRequired),
		errors.Is(err, auth.ErrSSORestricted):
		// 403: a GitHub login whose teams match no allow-listed org (and the
		// deployment uses GitHub team-gating).
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// ---- Request / response shapes ----

// AuthMeResponse is GET /api/auth/me: the caller's identity view — the
// account, the org→teams tree, and the active context its access token
// carries. /api/auth/me is the one route that builds the tree, and a tree
// it cannot build is answered as an error, never as an empty tree.
type AuthMeResponse struct {
	User          UserView      `json:"user"`
	Orgs          []OrgTreeView `json:"orgs"`
	ActiveOrg     string        `json:"active_org_id,omitempty"`
	ActiveOrgRole string        `json:"active_org_role,omitempty"`
	ActiveTeam    string        `json:"active_team_id,omitempty"`
	ActiveRole    string        `json:"active_role,omitempty"`
	// SessionLineage is the lineage of the refresh session the presented
	// access token belongs to — what POST /api/auth/logout?lineage= names, so
	// a client's pending sign-out ends that session and no other. Empty for a
	// credential that names no session (a personal access token).
	SessionLineage string `json:"session_lineage,omitempty"`
	// ExpiresAt is that session's bound as the server holds it now — the
	// machine's expireStale reads it to drop a pending sign-out's record
	// once the session it would end is gone by itself — and never the
	// access token's own short-lived exp. Empty for a credential that names
	// no session (a personal access token) and when the session is gone: a
	// revocation landed between the mint and this read.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// AuthSessionResponse is what every route that mints a credential answers —
// a login, a registration, a refresh, a password change or reset, a desktop
// exchange, a team or org switch: the account, the active context the new
// access token carries, its expiry, and — for a non-browser client — the
// token itself. It carries no org tree: the tree would be read from the
// store after the credential is minted, and a read failing there would
// answer an error for a credential already minted. A client reads the tree
// from GET /api/auth/me.
type AuthSessionResponse struct {
	User          UserView `json:"user"`
	ActiveOrg     string   `json:"active_org_id,omitempty"`
	ActiveOrgRole string   `json:"active_org_role,omitempty"`
	ActiveTeam    string   `json:"active_team_id,omitempty"`
	ActiveRole    string   `json:"active_role,omitempty"`
	AccessToken   string   `json:"access_token,omitempty"`
	ExpiresAt     string   `json:"expires_at,omitempty"`
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
	// UpdatedAt and LastLoginAt answer "is this account dormant, or was it
	// never able to sign in at all?" — the first question an operator asks
	// about an account that reports seeing nothing, and the one the admin
	// console could not answer because the fields existed in the store and
	// nothing exposed them. LastLoginAt is empty when the account has never
	// completed a sign-in.
	//
	// UpdatedAt is the ROW's last change, stamped by the store on every
	// write: an admin edit, but also a sign-in and a failed sign-in, which
	// move the login stamp and the brute-force counter. Dormancy is
	// LastLoginAt's question, not this field's.
	UpdatedAt   string `json:"updated_at,omitempty"`
	LastLoginAt string `json:"last_login_at,omitempty"`
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
	v := UserView{
		ID:           u.ID,
		Email:        u.Email,
		Name:         u.Name,
		Status:       string(u.Status),
		IsSuperAdmin: u.IsSuperAdmin,
		CreatedAt:    u.CreatedAt.Format(time.RFC3339),
	}
	// A zero UpdatedAt renders as year 1 rather than as "unknown" — rows
	// predating the field would read as edited in the year 1.
	if !u.UpdatedAt.IsZero() {
		v.UpdatedAt = u.UpdatedAt.Format(time.RFC3339)
	}
	if u.LastLoginAt != nil && !u.LastLoginAt.IsZero() {
		v.LastLoginAt = u.LastLoginAt.Format(time.RFC3339)
	}
	return v
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

// mintedSession is a credential a route minted: the account, the active
// context and access token, and — for a route that minted one — the
// refresh session.
type mintedSession struct {
	user           identity.User
	orgID          string
	orgRole        identity.OrgRole
	teamID         string
	role           identity.Role
	access         string
	accessExpires  time.Time
	refresh        string
	refreshExpires time.Time
}

func loginSession(res auth.LoginResult) mintedSession {
	return mintedSession{
		user:           res.User,
		orgID:          res.ActiveOrgID,
		orgRole:        res.ActiveOrgRole,
		teamID:         res.ActiveTeamID,
		role:           res.ActiveRole,
		access:         res.AccessToken,
		accessExpires:  res.AccessExpires,
		refresh:        res.RefreshToken,
		refreshExpires: res.RefreshExpires,
	}
}

// switchSession is a team or org switch's credential: a new access token
// on the session the caller already holds, so no refresh.
func switchSession(sw auth.SwitchResult) mintedSession {
	return mintedSession{
		user:          sw.User,
		orgID:         sw.Identity.OrgID,
		orgRole:       sw.Identity.OrgRole,
		teamID:        sw.Identity.TeamID,
		role:          sw.Identity.Role,
		access:        sw.AccessToken,
		accessExpires: sw.AccessExpires,
	}
}

// writeSession delivers a minted credential as an AuthSessionResponse: the
// cookies, then the body, whose access token only a non-browser client
// receives — a browser holds it in the HttpOnly cookie. Nothing here reads
// the store: the operation that minted the credential has committed, and
// its answer cannot fail after it.
func (s *Server) writeSession(w http.ResponseWriter, r *http.Request, m mintedSession) {
	s.setAuthCookies(w, m.access, m.accessExpires, m.refresh, m.refreshExpires)
	resp := AuthSessionResponse{
		User:          s.toUserView(m.user),
		ActiveOrg:     m.orgID,
		ActiveOrgRole: string(m.orgRole),
		ActiveTeam:    m.teamID,
		ActiveRole:    string(m.role),
		ExpiresAt:     m.accessExpires.Format(time.RFC3339),
	}
	if !isBrowserClient(r) {
		resp.AccessToken = m.access
	}
	writeJSON(w, resp)
}

// identityUnavailable is the body of every 5xx a store or server failure
// causes on the identity routes, which answer through authError and
// identityFailure: the /api/auth routes — the handlers of auth_sessions.go,
// auth_views.go, auth_oidc.go, password_routes.go, desktop_sso.go, the
// invitation lookup and accept, GET /api/auth/me and the two switches POST
// /api/auth/me/team/{team_id} and POST /api/auth/me/org/{org_id}
// (auth_teams.go), and GET /api/auth/oidc/github/callback/orgs
// (forge_github_orgs_routes.go, a forge feature under the auth prefix for
// its redirect URI, upstream exchange included) — plus POST
// /api/me/password, POST /api/me/sessions/revoke-all, the /api/me/sso
// routes, and GET and POST /api/teams and GET /api/teams/{id}/members, whose
// authorization reads answer here too when a "no" rests on a read that
// failed (canViewTeamOrFail, canManageOrgOrFail). An administrator's disable
// and password reset answer here when the revocation of the account's
// sessions fails. Any authenticated route reached with a personal access
// token answers here as well when one of the rows the PAT middleware
// resolves (up to four) cannot be read (identityFromPAT, patIdentityFailure),
// and so does a WebSocket upgrade whose ?ticket= the ticket store could not
// redeem (wsTicketIdentity). Such a failure's cause is a store or server error
// whose text names internals — a cluster's hosts, user ids — so it goes to
// the server log, never to the client. A deployment with no auth service
// answers "auth not configured" instead: a configuration, not a failure.
// The team-management routes (invitations, member changes) keep 500 bodies
// that name the row a failure may have left standing, for the operator to
// act on.
const identityUnavailable = "identity unavailable"

// authError answers err with the status mapAuthErrorStatus gives it: a
// refusal (4xx) carries its own message, a failure (5xx) is answered by
// identityFailure.
func (s *Server) authError(w http.ResponseWriter, r *http.Request, err error) {
	status := mapAuthErrorStatus(err)
	if status >= http.StatusInternalServerError {
		s.identityFailure(w, r, err)
		return
	}
	httpError(w, status, "%s", err.Error())
}

// identityFailure answers a failed store read or write of an auth or
// identity route: its cause is logged, the client gets 500 and
// identityUnavailable.
func (s *Server) identityFailure(w http.ResponseWriter, r *http.Request, err error) {
	if s.logger != nil {
		s.logger.Error("auth: %s %s: %v", r.Method, r.URL.Path, err)
	}
	httpError(w, http.StatusInternalServerError, "%s", identityUnavailable)
}

// buildOrgTree assembles the org→teams view for a user: every org they
// belong to (via OrgMembership, plus any org reachable through a team
// grant), each carrying the teams in it they can access. Org admins see
// every team in their org; plain members see only their granted teams.
//
// Every read here feeds the one tree, so the first store failure aborts
// the build and returns the error: a read that failed is not a fact
// about the user, and rendering it as an empty tree would answer
// "you belong to no organization" for an account that may belong to
// several. One org whose teams cannot be read fails the whole tree: no
// partial tree is rendered. Only reads that SUCCEED and find nothing
// render as empty — an org membership whose org row is gone is skipped,
// since the store said that org is not there.
func (s *Server) buildOrgTree(ctx context.Context, userID string) ([]OrgTreeView, error) {
	st := s.authStore()
	if st == nil {
		return nil, nil
	}
	// Org membership is the source of truth for which orgs a user can see:
	// every team grant mirrors up to an org membership (signup, invite, SSO,
	// migration), so iterating org memberships covers every reachable org
	// without a GetTeam per team grant.
	orgMems, err := st.ListOrgMembershipsByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("build org tree: list org memberships for user %s: %w", userID, err)
	}
	teamMems, err := st.ListMembershipsByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("build org tree: list team memberships for user %s: %w", userID, err)
	}
	teamRole := make(map[string]identity.Role, len(teamMems))
	for _, m := range teamMems {
		teamRole[m.TeamID] = m.Role
	}
	orgIDs := make([]string, 0, len(orgMems))
	for _, om := range orgMems {
		orgIDs = append(orgIDs, om.OrgID)
	}
	// One bulk fetch instead of a GetOrg per membership. An org absent
	// from the map is skipped: the store said it is not there. A read
	// that FAILED is not a fact about the user, so it aborts the build
	// instead of rendering as an empty tree.
	orgsByID, err := st.GetOrgsByIDs(ctx, orgIDs)
	if err != nil {
		return nil, fmt.Errorf("build org tree: bulk-load orgs for user %s: %w", userID, err)
	}
	out := make([]OrgTreeView, 0, len(orgMems))
	for _, om := range orgMems {
		org, ok := orgsByID[om.OrgID]
		if !ok {
			continue
		}
		orgRole := om.Role
		teams, err := st.ListTeamsByOrg(ctx, om.OrgID)
		if err != nil {
			return nil, fmt.Errorf("build org tree: list teams of org %s for user %s: %w", om.OrgID, userID, err)
		}
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
		errors.Is(err, auth.ErrSessionNotFound),
		errors.Is(err, auth.ErrResetNotFound),
		errors.Is(err, auth.ErrTokenExpired),
		errors.Is(err, auth.ErrTokenInvalid),
		errors.Is(err, auth.ErrTokenRevoked):
		return http.StatusUnauthorized
	case errors.Is(err, auth.ErrSignupClosed),
		errors.Is(err, auth.ErrInvitationMismatch),
		errors.Is(err, auth.ErrPasswordWeak),
		errors.Is(err, identity.ErrInvalidRole):
		return http.StatusBadRequest
	case errors.Is(err, auth.ErrLinkRequiresConsent),
		errors.Is(err, auth.ErrLinkAlreadyOwned):
		// 409 Conflict: an account exists with the same email but we
		// refuse to auto-link the new SSO identity (the UI prompts the
		// user to log in with their password, then link the SSO
		// connection from settings), or the identity is another
		// account's.
		return http.StatusConflict
	case errors.Is(err, auth.ErrInvitationNotFound),
		errors.Is(err, auth.ErrTeamNotFound),
		errors.Is(err, auth.ErrOrgNotFound),
		errors.Is(err, identity.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, identity.ErrEmailAlreadyTaken),
		errors.Is(err, identity.ErrSlugAlreadyTaken),
		errors.Is(err, identity.ErrOrgSlugAlreadyTaken),
		errors.Is(err, identity.ErrRunnerPoolHeld),
		errors.Is(err, identity.ErrUserStatusChanged),
		errors.Is(err, identity.ErrInvitationUsed):
		return http.StatusConflict
	case errors.Is(err, identity.ErrInvitationExpired):
		return http.StatusGone
	case errors.Is(err, auth.ErrNotAMember),
		errors.Is(err, auth.ErrNotAnOrgMember),
		errors.Is(err, auth.ErrNoTeamInOrg),
		errors.Is(err, auth.ErrPasswordChangeRequired),
		errors.Is(err, auth.ErrSSORestricted),
		errors.Is(err, auth.ErrNotDerivable):
		// 403: a GitHub login whose teams match no allow-listed org (and the
		// deployment uses GitHub team-gating); a credential that may never
		// mint what the route mints (a personal access token at a switch).
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

// derivationError answers a refusal of auth.Service.CheckDerivation: 403 for
// a credential that may never mint what the route mints, 401 for a revoked
// session or a disabled or pending account, and a read that failed through
// identityFailure.
func (s *Server) derivationError(w http.ResponseWriter, r *http.Request, err error) {
	s.authError(w, r, err)
}

// credentialChangeError answers a failed password change, reset confirm or
// forced rotation. ErrPasswordChangedSignIn — the new password IS committed,
// the new session could not be opened — answers 409
// `password_changed_sign_in` with its own message and clears the cookies of
// the session the change ended, so the client sends its user to sign in
// with the new password instead of retrying into "invalid credentials"; the
// cause is logged. ErrCredentialChangeBusy answers 409
// `credential_change_busy`. Everything else goes through authError.
func (s *Server) credentialChangeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrPasswordChangedSignIn):
		if s.logger != nil {
			s.logger.Error("auth: %s %s: %v", r.Method, r.URL.Path, err)
		}
		s.clearAuthCookies(w)
		s.httpErrorCode(w, r, http.StatusConflict, "password_changed_sign_in", "%s", auth.ErrPasswordChangedSignIn.Error())
	case errors.Is(err, auth.ErrCredentialChangeBusy):
		s.httpErrorCode(w, r, http.StatusConflict, "credential_change_busy", "%s", auth.ErrCredentialChangeBusy.Error())
	default:
		s.authError(w, r, err)
	}
}

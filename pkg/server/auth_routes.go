package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/mail"
)

// oidcAgentBindingCookie is the per-flow HttpOnly cookie set at
// /api/auth/oidc/<provider>/start and verified at the matching
// /callback. RFC 9700 (OAuth 2.0 Security BCP) §4.7.1: the `state`
// parameter proves freshness/uniqueness but does NOT bind the flow to
// the user agent. Without this cookie an attacker who completes
// /start in their browser and lures the victim into the resulting
// callback pins the victim into the attacker's account on iterion
// (classic login-CSRF / session fixation against OAuth/OIDC).
//
// Path-scoped to /api/auth/oidc/ so unrelated requests don't carry
// the value. 10 min MaxAge matches the StateStore TTL.
const oidcAgentBindingCookie = "iterion_oidc_agent"

// SSO callback error codes. The OIDC callback is a top-level browser
// navigation, so failures must NOT render as a bare API error page —
// instead we redirect to the SPA login route with one of these stable
// codes in `?sso_error=`, and the SPA maps it to a friendly message +
// the right next step. The raw provider detail stays in the server log
// (handleOIDCCallback) and is never reflected to the browser.
const (
	ssoErrUnknownProvider  = "unknown_provider"
	ssoErrProviderDisabled = "disabled"
	ssoErrProviderReturned = "provider_error"
	ssoErrStateExpired     = "state_expired"
	ssoErrAgentBinding     = "agent_binding"
	ssoErrExchangeFailed   = "exchange_failed"
	ssoErrLinkRequired     = "link_required"
	ssoErrRestricted       = "restricted"
	ssoErrLoginFailed      = "login_failed"
)

// redirectSSOError aborts an OIDC callback by redirecting the browser to the
// SPA login screen with a stable `?sso_error=<code>` (plus optional context
// like the provider display name), so the SPA can render a clean banner
// instead of the user landing on a raw `400 Bad Request` API page. The target
// is always a server-built relative `/login` path — never anything derived
// from a user-supplied `next` — so this can't become an open redirect.
func redirectSSOError(w http.ResponseWriter, r *http.Request, code string, extra url.Values) {
	q := url.Values{}
	q.Set("sso_error", code)
	for k, vs := range extra {
		for _, v := range vs {
			if v != "" {
				q.Add(k, v)
			}
		}
	}
	http.Redirect(w, r, "/login?"+q.Encode(), http.StatusFound)
}

// ssoErrorForAuth maps an auth-service login error to its SPA error code.
func ssoErrorForAuth(err error) string {
	switch {
	case errors.Is(err, auth.ErrLinkRequiresConsent):
		return ssoErrLinkRequired
	case errors.Is(err, auth.ErrSSORestricted):
		return ssoErrRestricted
	default:
		return ssoErrLoginFailed
	}
}

// registerAuthRoutes wires every /api/auth/* and /api/teams/*
// endpoint. Called from routes() when AuthService is non-nil.
func (s *Server) registerAuthRoutes() {
	if s.authLimiter == nil {
		s.authLimiter = newAuthRateLimiter()
	}
	// Per-route token-bucket rate limits (F-C1). Conservative bursts
	// so a legitimate user with sticky-keyboard / multiple devices
	// isn't surprised, but distributed brute-force is throttled.
	loginLimit := s.limitRoute(
		authBucketCfg{rate: 1.0 / 12.0, burst: 5}, // 5/min sustained, burst 5
		func(r *http.Request) string {
			// Second tier: rate-limit by email so distributed IPs
			// hammering one account also throttle. Extracted as a
			// pre-flight peek; if the body can't be parsed we fall
			// back to IP-only — the handler will return 400 anyway.
			email := peekJSONField(r, "email")
			if email == "" {
				return ""
			}
			return "email:" + strings.ToLower(email)
		},
	)
	registerLimit := s.limitRoute(
		authBucketCfg{rate: 1.0 / 30.0, burst: 3}, // 2/min sustained
		nil,
	)
	refreshLimit := s.limitRoute(
		authBucketCfg{rate: 1.0 / 2.0, burst: 30}, // 30/min — normal under long sessions
		nil,
	)
	// Anonymous routes (public via isPublicPath).
	s.mux.HandleFunc("POST /api/auth/login", loginLimit(s.handleLogin))
	// Complete a forced password rotation for a pending_password_change
	// account (e.g. the bootstrapped super-admin). Public + login-rate-limited
	// because the user holds no session until they have rotated.
	s.mux.HandleFunc("POST /api/auth/password/change", loginLimit(s.handleChangePassword))
	// Self-service reset: request is anti-enumeration (always 200) and
	// shares the login bucket so it can't be abused as an email cannon;
	// confirm redeems the one-shot emailed token.
	s.mux.HandleFunc("POST /api/auth/password/reset/request", loginLimit(s.handlePasswordResetRequest))
	s.mux.HandleFunc("POST /api/auth/password/reset/confirm", loginLimit(s.handlePasswordResetConfirm))
	s.mux.HandleFunc("POST /api/auth/register", registerLimit(s.handleRegister))
	s.mux.HandleFunc("POST /api/auth/refresh", refreshLimit(s.handleRefresh))
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/auth/providers", s.handleListProviders)
	s.mux.HandleFunc("GET /api/auth/oidc/{provider}/start", s.handleOIDCStart)
	s.mux.HandleFunc("GET /api/auth/oidc/{provider}/callback", s.handleOIDCCallback)
	// Desktop SSO: redeem a single-use ticket (minted by the OIDC callback for
	// a desktop flow) for tokens. Public + login-rate-limited — it is pre-auth
	// and the ticket is the sole credential.
	s.mux.HandleFunc("POST /api/auth/desktop/exchange", loginLimit(s.handleDesktopExchange))
	// WS ticket: authenticated caller mints a single-use ticket to open a WS
	// with ?ticket= instead of a JWT-in-URL. NOT public — it runs behind
	// requireAuth so the identity is already resolved.
	s.mux.HandleFunc("POST /api/ws/ticket", s.handleWSTicket)
	s.mux.HandleFunc("GET /api/auth/invitations/lookup", s.handleInvitationLookup)
	s.mux.HandleFunc("POST /api/auth/invitations/accept", s.handleInvitationAcceptForLoggedIn)

	// Authenticated routes.
	s.mux.Handle("GET /api/auth/me", s.requireAuth(http.HandlerFunc(s.handleMe)))
	s.mux.Handle("POST /api/auth/me/team/{team_id}", s.requireAuth(http.HandlerFunc(s.handleSwitchTeam)))
	s.mux.Handle("POST /api/auth/me/org/{org_id}", s.requireAuth(http.HandlerFunc(s.handleSwitchOrg)))
	s.mux.Handle("POST /api/me/password", s.requireAuth(http.HandlerFunc(s.handleChangeMyPassword)))
	s.mux.Handle("POST /api/me/sessions/revoke-all", s.requireAuth(http.HandlerFunc(s.handleRevokeAllSessions)))
	// Connected SSO identities (self-service): list, connect a new one (the exit
	// from the 409 link-required dead-end), and disconnect one.
	s.mux.Handle("GET /api/me/sso/links", s.requireAuth(http.HandlerFunc(s.handleListMySSOLinks)))
	s.mux.Handle("GET /api/me/sso/{provider}/link/start", s.requireAuth(http.HandlerFunc(s.handleOIDCLinkStart)))
	s.mux.Handle("DELETE /api/me/sso/links/{provider}/{subject}", s.requireAuth(http.HandlerFunc(s.handleUnlinkMySSO)))

	// Team management.
	s.mux.Handle("GET /api/teams", s.requireAuth(http.HandlerFunc(s.handleListTeams)))
	s.mux.Handle("POST /api/teams", s.requireAuth(http.HandlerFunc(s.handleCreateTeam)))
	s.mux.Handle("GET /api/teams/{id}/members", s.requireAuth(http.HandlerFunc(s.handleListTeamMembers)))
	s.mux.Handle("POST /api/teams/{id}/invitations", s.requireAuth(http.HandlerFunc(s.handleCreateInvitation)))
	s.mux.Handle("GET /api/teams/{id}/invitations", s.requireAuth(http.HandlerFunc(s.handleListInvitations)))
	s.mux.Handle("DELETE /api/teams/{id}/invitations/{invite_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteInvitation)))
	s.mux.Handle("PATCH /api/teams/{id}/members/{user_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateMember)))
	s.mux.Handle("DELETE /api/teams/{id}/members/{user_id}", s.requireAuth(http.HandlerFunc(s.handleRemoveMember)))

	// Org self-service (members / invitations / usage / teams). SSO and
	// audit org routes are registered by their own files.
	s.registerOrgRoutes()

	// Super-admin only.
	s.mux.Handle("GET /api/admin/users", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminListUsers)))
	s.mux.Handle("PATCH /api/admin/users/{id}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminUpdateUser)))
	s.mux.Handle("POST /api/admin/users/{id}/reset-password", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminResetUserPassword)))
	s.registerAdminOrgRoutes()
}

// ---- Request / response shapes ----

type authResponse struct {
	User          userView      `json:"user"`
	Orgs          []orgTreeView `json:"orgs"`
	ActiveOrg     string        `json:"active_org_id,omitempty"`
	ActiveOrgRole string        `json:"active_org_role,omitempty"`
	ActiveTeam    string        `json:"active_team_id,omitempty"`
	ActiveRole    string        `json:"active_role,omitempty"`
	AccessToken   string        `json:"access_token,omitempty"`
	ExpiresAt     string        `json:"expires_at,omitempty"`
}

// orgTreeView is one organization the user belongs to, with the teams
// inside it they can access. It is the shape /api/auth/me returns so
// the SPA can render an Org picker → Team picker without extra calls.
type orgTreeView struct {
	OrgID    string           `json:"org_id"`
	OrgName  string           `json:"org_name"`
	OrgSlug  string           `json:"org_slug"`
	OrgRole  string           `json:"org_role"`
	Personal bool             `json:"personal,omitempty"`
	Teams    []membershipView `json:"teams"`
}

type userView struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	Name         string `json:"name,omitempty"`
	Status       string `json:"status"`
	IsSuperAdmin bool   `json:"is_super_admin"`
	CreatedAt    string `json:"created_at,omitempty"`
}

type membershipView struct {
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

func (s *Server) toUserView(u identity.User) userView {
	return userView{
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
	resp := authResponse{
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
func (s *Server) buildOrgTree(ctx context.Context, userID string) ([]orgTreeView, error) {
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
	out := make([]orgTreeView, 0, len(orgMems))
	for _, om := range orgMems {
		org, err := st.GetOrg(ctx, om.OrgID)
		if err != nil {
			continue
		}
		orgRole := om.Role
		teams, _ := st.ListTeamsByOrg(ctx, om.OrgID)
		tv := make([]membershipView, 0, len(teams))
		for _, t := range teams {
			role, granted := teamRole[t.ID]
			if !granted {
				// Org admins can see/manage every team in their org.
				if !orgRole.AtLeast(identity.OrgRoleAdmin) {
					continue
				}
				role = identity.RoleAdmin
			}
			tv = append(tv, membershipView{
				TeamID:   t.ID,
				TeamName: t.Name,
				TeamSlug: t.Slug,
				Role:     string(role),
				Personal: t.Personal,
			})
		}
		out = append(out, orgTreeView{
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
	resp := authResponse{
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

func (s *Server) setAuthCookies(w http.ResponseWriter, access string, accessExp time.Time, refresh string, refreshExp time.Time) {
	access = strings.TrimSpace(access)
	if access != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     authCookieName,
			Value:    access,
			Path:     "/",
			Domain:   s.cfg.CookieDomain,
			HttpOnly: true,
			Secure:   s.cfg.CookieSecure,
			SameSite: http.SameSiteLaxMode,
			Expires:  accessExp,
		})
	}
	if refresh != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     refreshCookieName,
			Value:    refresh,
			Path:     "/api/auth",
			Domain:   s.cfg.CookieDomain,
			HttpOnly: true,
			Secure:   s.cfg.CookieSecure,
			SameSite: http.SameSiteLaxMode,
			Expires:  refreshExp,
		})
	}
}

func (s *Server) clearAuthCookies(w http.ResponseWriter) {
	for _, name := range []string{authCookieName, refreshCookieName} {
		path := "/"
		if name == refreshCookieName {
			path = "/api/auth"
		}
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     path,
			Domain:   s.cfg.CookieDomain,
			HttpOnly: true,
			Secure:   s.cfg.CookieSecure,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})
	}
}

func (s *Server) refreshTokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie(refreshCookieName); err == nil && c != nil {
		return c.Value
	}
	// Fallback for SDK clients that send it in the body via header.
	if h := r.Header.Get("X-Iterion-Refresh"); h != "" {
		return h
	}
	return ""
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

// ---- Anonymous handlers ----

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" || req.Password == "" {
		httpError(w, http.StatusBadRequest, "email and password required")
		return
	}
	res, err := s.authSvc.Login(r.Context(), req.Email, req.Password, r.UserAgent(), s.clientIP(r))
	if err != nil {
		// Collapse "account disabled" and "invalid credentials" to the
		// same wire message so an attacker can't enumerate which
		// addresses correspond to disabled accounts. The detailed err
		// stays available in logs.
		if errors.Is(err, auth.ErrAccountDisabled) || errors.Is(err, auth.ErrInvalidCredentials) {
			s.markLogin("invalid")
			httpError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		// Password verified but pending_password_change status: surface
		// the explicit signal so the SPA routes to the change-password
		// flow. Don't mint cookies here — issuing tokens before the
		// password is rotated would defeat the gate entirely.
		if errors.Is(err, auth.ErrPasswordChangeRequired) {
			s.markLogin("password_change_required")
			httpError(w, http.StatusForbidden, "password change required")
			return
		}
		// Lockout deliberately surfaces as ErrInvalidCredentials above
		// (timing-indistinguishable), so there is no separate "locked"
		// label — anything else is an internal error.
		s.markLogin("error")
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.markLogin("success")
	s.renderAuthResponse(w, r, res)
}

// markLogin bumps the password-login outcome counter (no-op without a
// metrics registry).
func (s *Server) markLogin(result string) {
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.AuthLoginsTotal.WithLabelValues(result).Inc()
	}
}

// handleChangePassword completes the forced-rotation flow for a
// pending_password_change account: verify the temp password, set the new
// one, activate, and return a session. Errors map opaquely (401 for a bad
// email/temp/status, 400 for a too-weak new password) so the endpoint can't
// be used to probe account existence or state.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" || req.CurrentPassword == "" || req.NewPassword == "" {
		httpError(w, http.StatusBadRequest, "email, current_password and new_password required")
		return
	}
	res, err := s.authSvc.ChangePasswordPending(r.Context(), req.Email, req.CurrentPassword, req.NewPassword, r.UserAgent(), s.clientIP(r))
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.renderAuthResponse(w, r, res)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" || req.Password == "" {
		httpError(w, http.StatusBadRequest, "email and password required")
		return
	}
	res, err := s.authSvc.Register(r.Context(), req.Email, req.Password, req.Name, req.Invitation, r.UserAgent(), s.clientIP(r))
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.renderAuthResponse(w, r, res)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	tok := s.refreshTokenFromRequest(r)
	if tok == "" {
		httpError(w, http.StatusUnauthorized, "no refresh token")
		return
	}
	res, err := s.authSvc.Refresh(r.Context(), tok, r.UserAgent(), s.clientIP(r))
	if err != nil {
		s.clearAuthCookies(w)
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.renderAuthResponse(w, r, res)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	tok := s.refreshTokenFromRequest(r)
	if tok != "" {
		_ = s.authSvc.Logout(r.Context(), tok)
	}
	s.clearAuthCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	type provider struct {
		Name    string `json:"name"`
		Display string `json:"display"`
	}
	out := struct {
		SignupMode string     `json:"signup_mode"`
		Providers  []provider `json:"providers"`
	}{SignupMode: s.cfg.SignupMode}
	seen := make(map[string]struct{})
	add := func(name, display string) {
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		out.Providers = append(out.Providers, provider{Name: name, Display: display})
	}
	if s.oidcRegistry != nil {
		for _, c := range s.oidcRegistry.Enabled() {
			add(c.Name(), c.Display())
		}
	}
	// Per-org providers (a tenant's own Keycloak). The org is resolved EITHER
	// from an explicit slug (?org=) OR — the friendlier default — from the
	// user's email/domain (?email= / ?domain=) via the org's verified domains,
	// so a user never has to know their org's slug. Both paths return only the
	// global providers for an unknown org — never 404 — so this anonymous
	// endpoint is not an org-existence oracle.
	if s.orgSSO != nil {
		for _, tenantID := range s.resolveOrgTenants(r) {
			rows, _ := s.orgSSO.ListByTenantKind(r.Context(), tenantID, orgsso.KindOIDC)
			for _, row := range rows {
				if !row.Enabled {
					continue
				}
				disp := row.DisplayName
				if disp == "" {
					disp = "SSO"
				}
				add(row.OIDCSlug(), disp)
			}
		}
	}
	writeJSON(w, out)
}

// resolveOrgTenants returns the tenant ids whose per-org SSO should be offered
// on the login screen, resolved from the request's ?org= slug and/or
// ?email=/?domain= (matched against verified domains). Best-effort and
// non-oracle: any miss yields no tenant rather than an error.
func (s *Server) resolveOrgTenants(r *http.Request) []string {
	var out []string
	seen := make(map[string]struct{})
	push := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	if org := strings.TrimSpace(r.URL.Query().Get("org")); org != "" {
		// SSO is org-level, but stored under the org's primary team
		// (the storage tenant) — resolve the public org slug to that team.
		if o, err := s.authStore().GetOrgBySlug(r.Context(), org); err == nil {
			push(s.firstTeamInOrg(r.Context(), o.ID))
		}
	}
	domain := strings.TrimSpace(r.URL.Query().Get("domain"))
	if domain == "" {
		domain = orgsso.EmailDomain(r.URL.Query().Get("email"))
	}
	if domain != "" && s.orgDomains != nil {
		if tenants, err := s.orgDomains.TenantsForDomain(r.Context(), domain); err == nil {
			for _, id := range tenants {
				push(id)
			}
		}
	}
	return out
}

// ---- OIDC handlers ----

func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	s.beginOIDCFlow(w, r, r.PathValue("provider"), "")
}

// handleOIDCLinkStart begins an SSO flow that, on completion, attaches the
// resolved identity to the already-authenticated caller (the exit from the 409
// "link requires consent" dead-end). It lives under /api/me/ — NOT the public
// /api/auth/oidc/ namespace — so requireAuth runs and the caller is known. The
// shared callback distinguishes the two via PendingAuth.LinkUserID.
func (s *Server) handleOIDCLinkStart(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok || id.UserID == "" {
		httpError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	s.beginOIDCFlow(w, r, r.PathValue("provider"), id.UserID)
}

// beginOIDCFlow resolves the connector, persists PendingAuth and redirects to
// the IdP authorize URL. linkUserID is empty for a sign-in flow and set to the
// caller's user id for the connect-from-settings flow.
func (s *Server) beginOIDCFlow(w http.ResponseWriter, r *http.Request, name, linkUserID string) {
	c, tenantID, providerID, err := s.resolveConnector(r.Context(), name)
	if err != nil {
		httpError(w, http.StatusNotFound, "unknown provider")
		return
	}
	state, verifier, _, err := oidc.GenerateStateAndPKCE()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	redirectURI := s.oidcRedirectURI(name)
	// A link flow always returns to settings; only a sign-in honours ?next=.
	// A DESKTOP sign-in instead validates ?next= as a loopback URL and, at the
	// callback, mints a single-use ticket + 302s there (see handleOIDCCallback
	// + desktop_sso.go) rather than setting browser cookies.
	next := ""
	desktop := false
	desktopRedirect := ""
	if linkUserID == "" {
		if r.URL.Query().Get("desktop") == "1" {
			desktopRedirect = safeDesktopRedirect(r.URL.Query().Get("next"))
			if desktopRedirect == "" {
				httpError(w, http.StatusBadRequest, "desktop flow requires a loopback next= URL")
				return
			}
			desktop = true
		} else {
			next = safeNext(r.URL.Query().Get("next"))
		}
	}
	authURL, err := c.AuthorizeURL(r.Context(), redirectURI, state, verifier)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "build authorize URL: %v", err)
		return
	}
	// The agent-binding cookie is a BROWSER login-CSRF guard: it is set on the
	// /start response and re-presented by the same browser at /callback. A
	// DESKTOP flow calls /start from its native client (so the cookie would go
	// there, not the browser that completes the callback) — binding is instead
	// guaranteed by the desktop-controlled loopback listener + the single-use
	// state + PKCE. So skip the cookie and store an empty AgentBinding, which
	// the callback treats as "no browser binding to check" (same as CLI/SDK).
	binding := ""
	if !desktop {
		binding, err = newAgentBindingToken()
		if err != nil {
			httpError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if err := s.oidcStates.Put(r.Context(), oidc.PendingAuth{
		Provider:        name,
		State:           state,
		CodeVerifier:    verifier,
		RedirectURI:     redirectURI,
		NextURL:         next,
		IssuedAt:        time.Now().UTC(),
		AgentBinding:    binding,
		TenantID:        tenantID,
		OrgProviderID:   providerID,
		LinkUserID:      linkUserID,
		Desktop:         desktop,
		DesktopRedirect: desktopRedirect,
	}); err != nil {
		httpError(w, http.StatusInternalServerError, "persist state: %v", err)
		return
	}
	if binding != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     oidcAgentBindingCookie,
			Value:    binding,
			Path:     "/api/auth/oidc/",
			Domain:   s.cfg.CookieDomain,
			HttpOnly: true,
			Secure:   s.cfg.CookieSecure,
			// SameSite=Lax is required: the callback is a top-level GET
			// navigation from the IdP and Strict would block the cookie.
			// Lax is sufficient because we additionally require the cookie
			// value to match PendingAuth.AgentBinding at /callback — a
			// cross-site script can't read the cookie (HttpOnly) and can't
			// set a cookie for iterion's origin (same-origin policy).
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int((10 * time.Minute).Seconds()),
		})
	}
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, map[string]string{"authorize_url": authURL})
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// newAgentBindingToken returns a 32-byte base64url-encoded random
// token used as the OIDC flow's user-agent binding cookie.
func newAgentBindingToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// clearOIDCAgentBindingCookie deletes the per-flow agent-binding
// cookie. Called at /callback (regardless of outcome) so each cookie
// is used at most once.
func clearOIDCAgentBindingCookie(w http.ResponseWriter, domain string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     oidcAgentBindingCookie,
		Value:    "",
		Path:     "/api/auth/oidc/",
		Domain:   domain,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	c, _, _, err := s.resolveConnector(r.Context(), name)
	if err != nil {
		// The callback is a top-level navigation, so surface failures as a
		// friendly SPA banner rather than a bare API error page.
		code := ssoErrUnknownProvider
		if errors.Is(err, oidc.ErrProviderDisabled) {
			code = ssoErrProviderDisabled
		}
		redirectSSOError(w, r, code, nil)
		return
	}
	provQ := url.Values{"sso_provider": {c.Display()}}
	if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
		// Don't reflect the provider's error_description verbatim:
		// some providers include server-side context (account ids,
		// internal flags) that we shouldn't surface to the SPA. The
		// short OAuth error code is sufficient for the UI; full
		// detail (if needed) is in the server log.
		if s.logger != nil {
			s.logger.Warn("oidc callback error from %s: code=%s description=%q",
				name, oauthErr, r.URL.Query().Get("error_description"))
		}
		redirectSSOError(w, r, ssoErrProviderReturned, provQ)
		return
	}
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if state == "" || code == "" {
		redirectSSOError(w, r, ssoErrStateExpired, provQ)
		return
	}
	pending, err := s.oidcStates.Take(r.Context(), state)
	if err != nil {
		redirectSSOError(w, r, ssoErrStateExpired, provQ)
		return
	}
	if pending.Provider != name {
		redirectSSOError(w, r, ssoErrStateExpired, provQ)
		return
	}
	// Verify the user-agent binding cookie matches the one issued at
	// /start (login-CSRF guard per RFC 9700 §4.7.1). Constant-time
	// compare avoids timing leaks on near-miss values. The cookie is
	// cleared regardless of outcome — single-use semantics.
	if pending.AgentBinding != "" {
		ck, cerr := r.Cookie(oidcAgentBindingCookie)
		clearOIDCAgentBindingCookie(w, s.cfg.CookieDomain, s.cfg.CookieSecure)
		if cerr != nil || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(pending.AgentBinding)) != 1 {
			redirectSSOError(w, r, ssoErrAgentBinding, provQ)
			return
		}
	}
	ext, err := c.ExchangeCode(r.Context(), code, pending.RedirectURI, pending.CodeVerifier)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("oidc exchange failed for %s: %v", name, err)
		}
		redirectSSOError(w, r, ssoErrExchangeFailed, provQ)
		return
	}
	// Link flow: an authenticated user is attaching this SSO identity to their
	// existing account (started from /link/start). Attach and bounce back to
	// settings instead of running login/signup.
	if pending.LinkUserID != "" {
		if err := s.authSvc.LinkExternalToUser(r.Context(), ext, pending.LinkUserID); err != nil {
			if s.logger != nil {
				s.logger.Warn("oidc link failed for user %s via %s: %v", pending.LinkUserID, name, err)
			}
			http.Redirect(w, r, "/settings?sso_link_error="+url.QueryEscape(linkErrorCode(err)), http.StatusFound)
			return
		}
		http.Redirect(w, r, "/settings?sso_linked="+url.QueryEscape(c.Display()), http.StatusFound)
		return
	}
	// Per-org flows drive the login from the tenant/provider stored in
	// PendingAuth at /start (server-side, keyed by the IdP-echoed state) — NOT
	// from the URL — so a slug presented at /callback cannot be coerced into
	// another tenant's policy.
	var res auth.LoginResult
	if pending.TenantID != "" {
		res, err = s.authSvc.LoginWithExternalForOrg(r.Context(), ext, pending.TenantID, pending.OrgProviderID, r.UserAgent(), s.clientIP(r))
	} else {
		res, err = s.authSvc.LoginWithExternal(r.Context(), ext, r.UserAgent(), s.clientIP(r))
	}
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("sso login failed via %s: %v", name, err)
		}
		redirectSSOError(w, r, ssoErrorForAuth(err), provQ)
		return
	}
	// Desktop flow: don't set browser cookies. Mint a single-use ticket holding
	// this LoginResult and 302 to the desktop's loopback listener with it; the
	// desktop redeems it at /api/auth/desktop/exchange over its native client
	// (see desktop_sso.go). The refresh token thus never enters a URL.
	if pending.Desktop {
		if s.desktopTickets == nil {
			redirectSSOError(w, r, ssoErrExchangeFailed, provQ)
			return
		}
		ticket, mintErr := s.desktopTickets.Mint(r.Context(), res)
		if mintErr != nil {
			redirectSSOError(w, r, ssoErrExchangeFailed, provQ)
			return
		}
		sep := "?"
		if strings.Contains(pending.DesktopRedirect, "?") {
			sep = "&"
		}
		http.Redirect(w, r, pending.DesktopRedirect+sep+"ticket="+url.QueryEscape(ticket), http.StatusFound)
		return
	}
	s.setAuthCookies(w, res.AccessToken, res.AccessExpires, res.RefreshToken, res.RefreshExpires)
	target := pending.NextURL
	if target == "" {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// linkErrorCode maps a link-flow error to the SPA settings banner code.
func linkErrorCode(err error) string {
	if errors.Is(err, auth.ErrLinkAlreadyOwned) {
		return "already_linked"
	}
	return "failed"
}

// ssoLinkView is the public shape of a connected SSO identity.
type ssoLinkView struct {
	Provider       string `json:"provider"`
	ProviderUserID string `json:"provider_user_id"`
	Email          string `json:"email,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
}

// handleListMySSOLinks returns the caller's connected SSO identities.
func (s *Server) handleListMySSOLinks(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	links, err := s.authSvc.ListSSOLinks(r.Context(), id.UserID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	out := struct {
		Links []ssoLinkView `json:"links"`
	}{Links: make([]ssoLinkView, 0, len(links))}
	for _, l := range links {
		v := ssoLinkView{Provider: l.Provider, ProviderUserID: l.ProviderUserID, Email: l.Email}
		if !l.CreatedAt.IsZero() {
			v.CreatedAt = l.CreatedAt.UTC().Format(time.RFC3339)
		}
		out.Links = append(out.Links, v)
	}
	writeJSON(w, out)
}

// handleUnlinkMySSO disconnects one of the caller's SSO identities.
func (s *Server) handleUnlinkMySSO(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	provider := r.PathValue("provider")
	subject := r.PathValue("subject")
	if provider == "" || subject == "" {
		httpError(w, http.StatusBadRequest, "provider and subject required")
		return
	}
	if err := s.authSvc.UnlinkExternal(r.Context(), id.UserID, provider, subject); err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) oidcRedirectURI(provider string) string {
	base := s.cfg.PublicURL
	if base == "" {
		// Local fallback: use the bound address.
		base = fmt.Sprintf("http://%s:%d", s.cfg.Bind, s.cfg.Port)
	}
	return base + "/api/auth/oidc/" + provider + "/callback"
}

// safeNext sanitizes the post-login redirect target to avoid open
// redirects: only same-origin, relative paths starting with "/" and
// not "//" are allowed.
func safeNext(v string) string {
	if v == "" {
		return ""
	}
	// Reject backslashes outright: WHATWG-compliant browsers fold "\" to
	// "/", so "/\evil.com" (which url.Parse reads as an empty-host path)
	// becomes "//evil.com" — a protocol-relative redirect to another
	// origin. Checking the raw input closes that normalization gap.
	if strings.ContainsRune(v, '\\') {
		return ""
	}
	// The raw value must start with a single "/": a leading "//" (or the
	// backslash variant above) is protocol-relative and escapes the origin.
	if !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil {
		return ""
	}
	if u.Scheme != "" || u.Host != "" {
		return ""
	}
	if !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return ""
	}
	return u.String()
}

// ---- Authenticated handlers ----

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	u, err := s.authStore().GetUser(r.Context(), id.UserID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "load user: %v", err)
		return
	}
	orgs, _ := s.buildOrgTree(r.Context(), u.ID)
	writeJSON(w, authResponse{
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
	views := make([]membershipView, 0, len(memberships))
	for _, m := range memberships {
		t, err := s.authStore().GetTeam(r.Context(), m.TeamID)
		if err != nil {
			continue
		}
		views = append(views, membershipView{
			TeamID:   t.ID,
			TeamName: t.Name,
			TeamSlug: t.Slug,
			Role:     string(m.Role),
			Personal: t.Personal,
		})
	}
	writeJSON(w, struct {
		Teams []membershipView `json:"teams"`
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
	out := make([]memberView, 0, len(ms))
	for _, m := range ms {
		u, _ := s.authStore().GetUser(r.Context(), m.UserID)
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
	mb.Role = role
	if err := s.authStore().UpsertMembership(r.Context(), mb); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
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
		bearer := extractBearer(r)
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

// ---- Admin handlers ----

func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	// ?offset / ?limit pagination — the previous hardcoded Page{Limit:
	// 200} silently truncated any deployment past 200 users.
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	switch {
	case limit <= 0:
		limit = 50
	case limit > 200:
		limit = 200
	}
	users, err := s.authStore().ListUsers(r.Context(), identity.Page{Offset: offset, Limit: limit})
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	views := make([]userView, 0, len(users))
	for _, u := range users {
		views = append(views, s.toUserView(u))
	}
	writeJSON(w, struct {
		Users  []userView `json:"users"`
		Offset int        `json:"offset"`
		Limit  int        `json:"limit"`
	}{Users: views, Offset: offset, Limit: limit})
}

func (s *Server) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	u, err := s.authStore().GetUser(r.Context(), id)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	var req adminUpdateUserReq
	if !decodeJSON(w, r, &req) {
		return
	}
	statusChangedToDisabled := false
	if req.Status != nil {
		switch identity.UserStatus(*req.Status) {
		case identity.UserStatusActive, identity.UserStatusDisabled, identity.UserStatusPendingPasswordChange:
			if u.Status != identity.UserStatusDisabled && identity.UserStatus(*req.Status) == identity.UserStatusDisabled {
				statusChangedToDisabled = true
			}
			u.Status = identity.UserStatus(*req.Status)
		default:
			httpError(w, http.StatusBadRequest, "invalid status")
			return
		}
	}
	if req.IsSuperAdmin != nil {
		u.IsSuperAdmin = *req.IsSuperAdmin
	}
	if req.Name != nil {
		u.Name = *req.Name
	}
	u.UpdatedAt = time.Now().UTC()
	if err := s.authStore().UpdateUser(r.Context(), u); err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	// On admin-disable, revoke every live refresh session so the user
	// loses access at the next access-token expiry (≤15 min) instead
	// of waiting for the existing refresh TTL (~30 days). Without this,
	// Refresh re-fetches the user but a TOCTOU window between
	// GetUser-Status check and the CAS-revoke (auth/service.go:282-293)
	// allows a 15-min access token to be minted after the admin
	// clicked "disable" — see F-CL-5 in docs/reviews/.
	if statusChangedToDisabled {
		if err := s.authSvc.RevokeUserSessions(r.Context(), u.ID); err != nil && s.logger != nil {
			s.logger.Warn("auth: revoke sessions on user %s disable: %v", u.ID, err)
		}
	}
	meta := map[string]any{"status": string(u.Status), "is_super_admin": u.IsSuperAdmin}
	if req.IsSuperAdmin != nil {
		meta["super_admin_changed"] = true
	}
	s.auditPlatform(r, "", "user.updated", "user", u.ID, meta)
	writeJSON(w, s.toUserView(u))
}

// handleAdminResetUserPassword mints a one-shot temporary password for
// a locked-out account: the hash replaces the user's password, the
// account flips to pending_password_change, and every live session is
// revoked. The plaintext is returned ONCE to the super-admin, who hands
// it to the user out-of-band; the first sign-in then goes through the
// forced-rotation flow with the temp password as "current password".
// This is the recovery path on deployments without outbound email.
func (s *Server) handleAdminResetUserPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	u, err := s.authStore().GetUser(r.Context(), id)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		httpError(w, http.StatusInternalServerError, "generate temp password: %s", err.Error())
		return
	}
	temp := base64.RawURLEncoding.EncodeToString(raw)
	hash, err := auth.HashPassword(temp)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "hash temp password: %s", err.Error())
		return
	}
	u.PasswordHash = hash
	u.Status = identity.UserStatusPendingPasswordChange
	u.UpdatedAt = time.Now().UTC()
	if err := s.authStore().UpdateUser(r.Context(), u); err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	// The old credential (and whoever holds it) must lose access now,
	// not at refresh-TTL expiry.
	if err := s.authSvc.RevokeUserSessions(r.Context(), u.ID); err != nil && s.logger != nil {
		s.logger.Warn("auth: revoke sessions on user %s password reset: %v", u.ID, err)
	}
	s.auditPlatform(r, "", "user.password_reset", "user", u.ID, nil)
	writeJSON(w, struct {
		TempPassword string `json:"temp_password"`
	}{TempPassword: temp})
}

// ---- Authorization helpers ----

func (s *Server) canViewTeam(ctx context.Context, id auth.Identity, teamID string) bool {
	// Synthetic principals (webhook / config-share) never authorize an
	// operator team action, even if a future bug set a role on one.
	if id.IsSynthetic() {
		return false
	}
	if id.IsSuperAdmin {
		return true
	}
	// AtLeast(RoleViewer) is equivalent to the old Valid() for the four ladder
	// roles, but excludes the orthogonal config_editor capability (rank 0) — a
	// config editor is not a team viewer (ADR-078); it reaches only its own
	// endpoints via canEditConfigShares.
	if id.TeamID == teamID && id.Role.AtLeast(identity.RoleViewer) {
		return true
	}
	if mb, err := s.authStore().GetMembership(ctx, id.UserID, teamID); err == nil && mb.Role.AtLeast(identity.RoleViewer) {
		return true
	}
	// An org admin/owner can view every team in their org.
	return s.orgAdminOfTeam(ctx, id, teamID)
}

// canEditConfigShares reports whether the principal may read/write this team's
// config-shares through the AUTHENTICATED (session) editor endpoints: the
// orthogonal config_editor capability on this team, plus anyone who already
// manages it (admin/owner, org admin, super-admin). Deliberately NOT
// canViewTeam — a plain viewer/member was never delegated config editing. The
// public iws_ token path has its own middleware; synthetic principals are
// rejected here. See ADR-078.
func (s *Server) canEditConfigShares(ctx context.Context, id auth.Identity, teamID string) bool {
	if id.IsSynthetic() {
		return false
	}
	if id.IsSuperAdmin {
		return true
	}
	if id.TeamID == teamID && id.Role == identity.RoleConfigEditor {
		return true
	}
	if mb, err := s.authStore().GetMembership(ctx, id.UserID, teamID); err == nil && mb.Role == identity.RoleConfigEditor {
		return true
	}
	return s.canManageTeam(ctx, id, teamID)
}

func (s *Server) canManageTeam(ctx context.Context, id auth.Identity, teamID string) bool {
	if id.IsSynthetic() {
		return false
	}
	if id.IsSuperAdmin {
		return true
	}
	if mb, err := s.authStore().GetMembership(ctx, id.UserID, teamID); err == nil && mb.Role.AtLeast(identity.RoleAdmin) {
		return true
	}
	// An org admin/owner can manage every team in their org.
	return s.orgAdminOfTeam(ctx, id, teamID)
}

// orgAdminOfTeam reports whether the principal is an admin/owner of the
// team's parent org (so org admins implicitly manage every team in it).
func (s *Server) orgAdminOfTeam(ctx context.Context, id auth.Identity, teamID string) bool {
	t, err := s.authStore().GetTeam(ctx, teamID)
	if err != nil || t.OrgID == "" {
		return false
	}
	return s.canManageOrg(ctx, id, t.OrgID)
}

// canViewOrg reports whether the principal may read an org's settings /
// roster / usage. Super-admins and any org member pass.
func (s *Server) canViewOrg(ctx context.Context, id auth.Identity, orgID string) bool {
	if id.IsSynthetic() {
		return false
	}
	if id.IsSuperAdmin {
		return true
	}
	if id.OrgID == orgID && id.OrgRole.Valid() {
		return true
	}
	om, err := s.authStore().GetOrgMembership(ctx, id.UserID, orgID)
	if err != nil {
		return false
	}
	return om.Role.Valid()
}

// canManageOrg reports whether the principal may mutate an org (members,
// SSO, settings, teams). Super-admins and org admins/owners pass.
func (s *Server) canManageOrg(ctx context.Context, id auth.Identity, orgID string) bool {
	if id.IsSynthetic() {
		return false
	}
	if id.IsSuperAdmin {
		return true
	}
	om, err := s.authStore().GetOrgMembership(ctx, id.UserID, orgID)
	if err != nil {
		return false
	}
	return om.Role.AtLeast(identity.OrgRoleAdmin)
}

// clientIP picks the audit IP for an inbound request. The default
// is r.RemoteAddr — the only field a client can't forge. We only
// consult X-Forwarded-For / X-Real-IP when the immediate peer is in
// s.cfg.TrustedProxyCIDRs, which the operator has explicitly
// configured. Without this guard a client could spoof its audit IP
// (and undermine any future IP-based throttling) just by sending an
// X-Forwarded-For header.
func (s *Server) clientIP(r *http.Request) string {
	if !s.peerIsTrusted(r) {
		return r.RemoteAddr
	}
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		if i := strings.Index(h, ","); i > 0 {
			return strings.TrimSpace(h[:i])
		}
		return strings.TrimSpace(h)
	}
	if h := r.Header.Get("X-Real-IP"); h != "" {
		return h
	}
	return r.RemoteAddr
}

func (s *Server) peerIsTrusted(r *http.Request) bool {
	if s == nil || len(s.cfg.TrustedProxyCIDRs) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, cidr := range s.cfg.TrustedProxyCIDRs {
		_, network, perr := net.ParseCIDR(cidr)
		if perr != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

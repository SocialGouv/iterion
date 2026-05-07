package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// registerAuthRoutes wires every /api/auth/* and /api/teams/*
// endpoint. Called from routes() when AuthService is non-nil.
func (s *Server) registerAuthRoutes() {
	// Anonymous routes (public via isPublicPath).
	s.mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/auth/register", s.handleRegister)
	s.mux.HandleFunc("POST /api/auth/refresh", s.handleRefresh)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/auth/providers", s.handleListProviders)
	s.mux.HandleFunc("GET /api/auth/oidc/{provider}/start", s.handleOIDCStart)
	s.mux.HandleFunc("GET /api/auth/oidc/{provider}/callback", s.handleOIDCCallback)
	s.mux.HandleFunc("GET /api/auth/invitations/lookup", s.handleInvitationLookup)
	s.mux.HandleFunc("POST /api/auth/invitations/accept", s.handleInvitationAcceptForLoggedIn)

	// Authenticated routes.
	s.mux.Handle("GET /api/auth/me", s.requireAuth(http.HandlerFunc(s.handleMe)))
	s.mux.Handle("POST /api/auth/me/team/{team_id}", s.requireAuth(http.HandlerFunc(s.handleSwitchTeam)))

	// Team management.
	s.mux.Handle("GET /api/teams", s.requireAuth(http.HandlerFunc(s.handleListTeams)))
	s.mux.Handle("POST /api/teams", s.requireAuth(http.HandlerFunc(s.handleCreateTeam)))
	s.mux.Handle("GET /api/teams/{id}/members", s.requireAuth(http.HandlerFunc(s.handleListTeamMembers)))
	s.mux.Handle("POST /api/teams/{id}/invitations", s.requireAuth(http.HandlerFunc(s.handleCreateInvitation)))
	s.mux.Handle("GET /api/teams/{id}/invitations", s.requireAuth(http.HandlerFunc(s.handleListInvitations)))
	s.mux.Handle("DELETE /api/teams/{id}/invitations/{invite_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteInvitation)))
	s.mux.Handle("PATCH /api/teams/{id}/members/{user_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateMember)))
	s.mux.Handle("DELETE /api/teams/{id}/members/{user_id}", s.requireAuth(http.HandlerFunc(s.handleRemoveMember)))

	// Super-admin only.
	s.mux.Handle("GET /api/admin/users", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminListUsers)))
	s.mux.Handle("PATCH /api/admin/users/{id}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminUpdateUser)))
}

// ---- Request / response shapes ----

type authResponse struct {
	User        userView         `json:"user"`
	Teams       []membershipView `json:"teams"`
	ActiveTeam  string           `json:"active_team_id,omitempty"`
	ActiveRole  string           `json:"active_role,omitempty"`
	AccessToken string           `json:"access_token,omitempty"`
	ExpiresAt   string           `json:"expires_at,omitempty"`
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

type registerReq struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	Name       string `json:"name,omitempty"`
	Invitation string `json:"invitation,omitempty"`
}

type createTeamReq struct {
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
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

func (s *Server) renderAuthResponse(w http.ResponseWriter, r *http.Request, res auth.LoginResult) {
	teams := make([]membershipView, 0, len(res.Memberships))
	for _, m := range res.Memberships {
		t, err := s.authStore().GetTeam(r.Context(), m.TeamID)
		if err != nil {
			continue
		}
		teams = append(teams, membershipView{
			TeamID:   t.ID,
			TeamName: t.Name,
			TeamSlug: t.Slug,
			Role:     string(m.Role),
			Personal: t.Personal,
		})
	}
	s.setAuthCookies(w, res.AccessToken, res.AccessExpires, res.RefreshToken, res.RefreshExpires)
	writeJSON(w, authResponse{
		User:        s.toUserView(res.User),
		Teams:       teams,
		ActiveTeam:  res.ActiveTeamID,
		ActiveRole:  string(res.ActiveRole),
		AccessToken: res.AccessToken,
		ExpiresAt:   res.AccessExpires.Format(time.RFC3339),
	})
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
		errors.Is(err, auth.ErrTokenInvalid):
		return http.StatusUnauthorized
	case errors.Is(err, auth.ErrSignupClosed),
		errors.Is(err, auth.ErrInvitationMismatch),
		errors.Is(err, auth.ErrPasswordWeak):
		return http.StatusBadRequest
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
	case errors.Is(err, auth.ErrNotAMember):
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

// ---- Anonymous handlers ----

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if req.Email == "" || req.Password == "" {
		httpError(w, http.StatusBadRequest, "email and password required")
		return
	}
	res, err := s.authSvc.Login(r.Context(), req.Email, req.Password, r.UserAgent(), clientIP(r))
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	s.renderAuthResponse(w, r, res)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if req.Email == "" || req.Password == "" {
		httpError(w, http.StatusBadRequest, "email and password required")
		return
	}
	res, err := s.authSvc.Register(r.Context(), req.Email, req.Password, req.Name, req.Invitation, r.UserAgent(), clientIP(r))
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
	res, err := s.authSvc.Refresh(r.Context(), tok, r.UserAgent(), clientIP(r))
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

func (s *Server) handleListProviders(w http.ResponseWriter, _ *http.Request) {
	type provider struct {
		Name    string `json:"name"`
		Display string `json:"display"`
	}
	out := struct {
		SignupMode string     `json:"signup_mode"`
		Providers  []provider `json:"providers"`
	}{SignupMode: s.cfg.SignupMode}
	if s.oidcRegistry != nil {
		for _, c := range s.oidcRegistry.Enabled() {
			out.Providers = append(out.Providers, provider{Name: c.Name(), Display: c.Display()})
		}
	}
	writeJSON(w, out)
}

// ---- OIDC handlers ----

func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if s.oidcRegistry == nil {
		httpError(w, http.StatusNotFound, "oidc disabled")
		return
	}
	name := r.PathValue("provider")
	c, err := s.oidcRegistry.Get(name)
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
	next := safeNext(r.URL.Query().Get("next"))
	authURL, err := c.AuthorizeURL(r.Context(), redirectURI, state, verifier)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "build authorize URL: %v", err)
		return
	}
	if err := s.oidcStates.Put(r.Context(), oidc.PendingAuth{
		Provider:     name,
		State:        state,
		CodeVerifier: verifier,
		RedirectURI:  redirectURI,
		NextURL:      next,
		IssuedAt:     time.Now().UTC(),
	}); err != nil {
		httpError(w, http.StatusInternalServerError, "persist state: %v", err)
		return
	}
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, map[string]string{"authorize_url": authURL})
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidcRegistry == nil {
		httpError(w, http.StatusNotFound, "oidc disabled")
		return
	}
	name := r.PathValue("provider")
	c, err := s.oidcRegistry.Get(name)
	if err != nil {
		httpError(w, http.StatusNotFound, "unknown provider")
		return
	}
	if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
		httpError(w, http.StatusBadRequest, "oauth error: %s — %s", oauthErr, r.URL.Query().Get("error_description"))
		return
	}
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if state == "" || code == "" {
		httpError(w, http.StatusBadRequest, "missing state or code")
		return
	}
	pending, err := s.oidcStates.Take(r.Context(), state)
	if err != nil {
		httpError(w, http.StatusBadRequest, "state expired or invalid")
		return
	}
	if pending.Provider != name {
		httpError(w, http.StatusBadRequest, "state/provider mismatch")
		return
	}
	ext, err := c.ExchangeCode(r.Context(), code, pending.RedirectURI, pending.CodeVerifier)
	if err != nil {
		httpError(w, http.StatusBadRequest, "exchange: %v", err)
		return
	}
	res, err := s.authSvc.LoginWithExternal(r.Context(), ext, r.UserAgent(), clientIP(r))
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "sso login: %v", err)
		return
	}
	s.setAuthCookies(w, res.AccessToken, res.AccessExpires, res.RefreshToken, res.RefreshExpires)
	target := pending.NextURL
	if target == "" {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusFound)
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
	memberships, _ := s.authStore().ListMembershipsByUser(r.Context(), u.ID)
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
	writeJSON(w, authResponse{
		User:       s.toUserView(u),
		Teams:      views,
		ActiveTeam: id.TeamID,
		ActiveRole: string(id.Role),
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
	writeJSON(w, authResponse{
		User:        s.toUserView(identity.User{ID: newID.UserID, Email: newID.Email}),
		ActiveTeam:  newID.TeamID,
		ActiveRole:  string(newID.Role),
		AccessToken: access,
		ExpiresAt:   exp.Format(time.RFC3339),
	})
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
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name required")
		return
	}
	t, err := s.authSvc.CreateTeamFor(r.Context(), id.UserID, req.Name, req.Slug)
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
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
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
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
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
	if err := readJSON(r, &body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
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
	writeJSON(w, mb)
}

// ---- Admin handlers ----

func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.authStore().ListUsers(r.Context(), identity.Page{Limit: 200})
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	views := make([]userView, 0, len(users))
	for _, u := range users {
		views = append(views, s.toUserView(u))
	}
	writeJSON(w, struct {
		Users []userView `json:"users"`
	}{Users: views})
}

func (s *Server) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	u, err := s.authStore().GetUser(r.Context(), id)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	var req adminUpdateUserReq
	if err := readJSON(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if req.Status != nil {
		switch identity.UserStatus(*req.Status) {
		case identity.UserStatusActive, identity.UserStatusDisabled, identity.UserStatusPendingPasswordChange:
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
	writeJSON(w, s.toUserView(u))
}

// ---- Authorization helpers ----

func (s *Server) canViewTeam(ctx context.Context, id auth.Identity, teamID string) bool {
	if id.IsSuperAdmin {
		return true
	}
	if id.TeamID == teamID {
		return id.Role.Valid()
	}
	mb, err := s.authStore().GetMembership(ctx, id.UserID, teamID)
	if err != nil {
		return false
	}
	return mb.Role.Valid()
}

func (s *Server) canManageTeam(ctx context.Context, id auth.Identity, teamID string) bool {
	if id.IsSuperAdmin {
		return true
	}
	mb, err := s.authStore().GetMembership(ctx, id.UserID, teamID)
	if err != nil {
		return false
	}
	return mb.Role.AtLeast(identity.RoleAdmin)
}

func clientIP(r *http.Request) string {
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

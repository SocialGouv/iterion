package server

import (
	"net/http"
	"strings"
)

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

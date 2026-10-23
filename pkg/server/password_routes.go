package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth"
)

// handlePasswordResetRequest mints + emails a one-shot reset link.
// ALWAYS 200 with the same body — account enumeration through this
// endpoint must be impossible (the service logs the real outcome).
func (s *Server) handlePasswordResetRequest(w http.ResponseWriter, r *http.Request) {
	if s.authSvc == nil {
		httpError(w, http.StatusServiceUnavailable, "auth not configured")
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if err := readJSON(r, &req); err != nil || req.Email == "" {
		httpError(w, http.StatusBadRequest, "email required")
		return
	}
	_ = s.authSvc.RequestPasswordReset(r.Context(), req.Email)
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.AuthPasswordResetsTotal.WithLabelValues("requested").Inc()
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// passwordResetConfirmReq is the body of POST /api/auth/password/reset/confirm.
type passwordResetConfirmReq struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// changeMyPasswordReq is the body of POST /api/me/password.
type changeMyPasswordReq struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handlePasswordResetConfirm redeems the emailed token: new password,
// every prior session revoked, fresh login issued.
func (s *Server) handlePasswordResetConfirm(w http.ResponseWriter, r *http.Request) {
	if s.authSvc == nil {
		httpError(w, http.StatusServiceUnavailable, "auth not configured")
		return
	}
	var req passwordResetConfirmReq
	if err := readJSON(r, &req); err != nil || req.Token == "" || req.Password == "" {
		httpError(w, http.StatusBadRequest, "token + password required")
		return
	}
	res, err := s.authSvc.ConfirmPasswordReset(r.Context(), req.Token, req.Password, r.UserAgent(), s.clientIP(r))
	if err != nil {
		s.credentialChangeError(w, r, err)
		return
	}
	s.auditPlatform(r, res.ActiveTeamID, "user.password_reset", "user", res.User.ID, nil)
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.AuthPasswordResetsTotal.WithLabelValues("confirmed").Inc()
	}
	s.writeSession(w, r, loginSession(res))
}

// handleChangeMyPassword is the authenticated self-service rotation.
// Other sessions are revoked; this one continues via the re-issued
// cookies in the response. A personal access token is refused (403): the
// proof is the current password, but the route answers with a SESSION —
// a pinned, non-minting credential must not be upgraded into one that
// mints. The caller signs in and proves the password there.
func (s *Server) handleChangeMyPassword(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	if id.Credential == auth.CredentialPAT {
		httpError(w, http.StatusForbidden, "a personal access token cannot change the account password: sign in to change it")
		return
	}
	var req changeMyPasswordReq
	if err := readJSON(r, &req); err != nil || req.CurrentPassword == "" || req.NewPassword == "" {
		httpError(w, http.StatusBadRequest, "current_password + new_password required")
		return
	}
	res, err := s.authSvc.ChangePassword(r.Context(), id.UserID, req.CurrentPassword, req.NewPassword, r.UserAgent(), s.clientIP(r))
	if err != nil {
		s.credentialChangeError(w, r, err)
		return
	}
	s.auditTenant(r, id.TeamID, "user.password_changed", "user", id.UserID, nil)
	s.writeSession(w, r, loginSession(res))
}

// handleRevokeAllSessions is "sign out everywhere", the account's compromise
// response: every refresh session ends (its record landed: each is refused
// at its next refresh, its access tokens at their next derivation, and they
// keep the access they grant until their own expiry, at most the access
// TTL), then what the sessions minted that outlives them ends too
// (endWhatSessionsMinted): every personal access token of the account is
// revoked and every SSO link a session wrote is removed — the one way to end
// a credential minted from a session before its revocation. The caller's own
// cookies are cleared too. The route runs behind the chokepoint, so a step
// that fails after the record answers the failure and the caller signs in
// again to present the request again: every step is idempotent.
func (s *Server) handleRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	if err := s.authSvc.RevokeUserSessions(r.Context(), id.UserID); err != nil {
		s.identityFailure(w, r, err)
		return
	}
	revokedPATs, removedLinks, err := s.endWhatSessionsMinted(r.Context(), id.UserID)
	if err != nil {
		// The revocation's RECORD landed (the caller's session is already
		// refused), a later step failed: the caller can no longer retry from
		// this session. The code tells the client to have the user sign in
		// again and repeat the request — every step is idempotent.
		s.httpErrorCode(w, r, http.StatusServiceUnavailable, "signout_cleanup_failed",
			"signed out; cleanup incomplete (%v) — sign in again and repeat sign out everywhere", err)
		return
	}
	s.auditTenant(r, id.TeamID, "user.sessions_revoked", "user", id.UserID, map[string]any{
		"personal_access_tokens_revoked": revokedPATs,
		"sso_links_removed":              removedLinks,
	})
	s.clearAuthCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

// endWhatSessionsMinted ends what the account's sessions minted that outlives
// them, once the caller recorded the account's session revocation ("sign out
// everywhere", an admin disable): every personal access token of the account
// is revoked, and every SSO link a signed-in session wrote is removed — a link
// an SSO sign-in wrote itself stays (auth.Service.RemoveSessionSSOLinks). It
// answers how many of each it ended; a failure names the step that failed.
func (s *Server) endWhatSessionsMinted(ctx context.Context, userID string) (pats, links int64, err error) {
	pats, err = s.revokeAccountPATs(ctx, userID)
	if err != nil {
		return 0, 0, fmt.Errorf("revoke the personal access tokens of user %s: %w", userID, err)
	}
	links, err = s.authSvc.RemoveSessionSSOLinks(ctx, userID)
	if err != nil {
		return pats, 0, err
	}
	return pats, links, nil
}

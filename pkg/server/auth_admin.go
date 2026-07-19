package server

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

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

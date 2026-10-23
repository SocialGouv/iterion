package server

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

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
	// ?q narrows the page to the accounts whose email starts with it, or
	// the one whose id equals it. Without it a console past a few dozen
	// users can only page, which is how the 17/09 investigation had to
	// find one account among the platform's.
	query := strings.TrimSpace(q.Get("q"))
	users, err := s.authStore().ListUsers(r.Context(), identity.UserFilter{
		Page:  identity.Page{Offset: offset, Limit: limit},
		Query: query,
	})
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	views := make([]UserView, 0, len(users))
	for _, u := range users {
		views = append(views, s.toUserView(u))
	}
	writeJSON(w, struct {
		Users  []UserView `json:"users"`
		Offset int        `json:"offset"`
		Limit  int        `json:"limit"`
		Query  string     `json:"query,omitempty"`
	}{Users: views, Offset: offset, Limit: limit, Query: query})
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
	// The console owns the status, the privilege flag and the display name —
	// and nothing else: a whole-row write would carry the password hash this
	// read predates, restoring a credential a concurrent reset just rotated.
	patch := identity.UserPatch{Name: req.Name, IsSuperAdmin: req.IsSuperAdmin}
	// Whether this request disables the account is decided from the REQUEST,
	// never from the row read above: a re-enable and a sign-in committed
	// inside that window would make the read say "already disabled" and the
	// landing deactivation skip the revocation below. Revoking the sessions
	// of an account that was already disabled is idempotent.
	//
	// A disable is the only transition here that revokes live sessions:
	// pending_password_change leaves them unrevoked but refuses their next
	// refresh and every credential they would mint, refuses an SSO sign-in
	// and sends a password sign-in to the rotation flow
	// (docs/cloud-admin-guide.md §1.6); the variant that also revokes is
	// reset-password below, which replaces the credential too.
	disabling := false
	if req.Status != nil {
		next := identity.UserStatus(*req.Status)
		switch next {
		case identity.UserStatusActive, identity.UserStatusDisabled, identity.UserStatusPendingPasswordChange:
			disabling = next == identity.UserStatusDisabled
			patch.Status = &next
		default:
			httpError(w, http.StatusBadRequest, "invalid status")
			return
		}
	}
	// No precondition: the super-admin console is the authority on a status,
	// not a reader of one.
	u, err = s.authStore().PatchUser(r.Context(), u.ID, patch)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	meta := map[string]any{"status": string(u.Status), "is_super_admin": u.IsSuperAdmin}
	if req.IsSuperAdmin != nil {
		meta["super_admin_changed"] = true
	}
	s.auditPlatform(r, "", "user.updated", "user", u.ID, meta)
	// On admin-disable, every live refresh session and every personal access
	// token is revoked, and every SSO link a session wrote is removed, AFTER
	// the status lands, so a sign-in committed before the deactivation dies
	// with it too; from the landing on, no sign-in, refresh, switch or
	// credential mint of the account succeeds, its personal access tokens are
	// refused, and its access tokens keep the access they grant until their
	// own expiry (at most the access TTL). The revocation is what keeps a
	// later re-enable from resurrecting them: one that fails is answered as
	// the failure it is — the account stays disabled — and the console's
	// retry revokes again.
	if disabling {
		if err := s.authSvc.RevokeUserSessions(r.Context(), u.ID); err != nil {
			s.identityFailure(w, r, fmt.Errorf("revoke the sessions of user %s after disabling it: %w", u.ID, err))
			return
		}
		if _, _, err := s.endWhatSessionsMinted(r.Context(), u.ID); err != nil {
			s.identityFailure(w, r, fmt.Errorf("after disabling user %s: %w", u.ID, err))
			return
		}
	}
	writeJSON(w, s.toUserView(u))
}

// handleAdminResetUserPassword mints a one-shot temporary password for
// a locked-out account: the hash replaces the user's password, the
// account flips to pending_password_change, and every live session is
// revoked. The plaintext is returned ONCE to the super-admin, who hands
// it to the user out-of-band; the first sign-in then goes through the
// forced-rotation flow with the temp password as "current password".
// This is the recovery path on deployments without outbound email.
// A disabled account must be explicitly re-enabled before this recovery.
func (s *Server) handleAdminResetUserPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	u, err := s.authStore().GetUser(r.Context(), id)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	// requireSuperAdmin asked the chokepoint for platform authority. A
	// temporary password for the caller's OWN account is a credential minted
	// for it: a personal access token may not mint one (it would outlive the
	// token's expiry and revocation), so the chokepoint is asked again for
	// that mint.
	if caller, _ := auth.FromContext(r.Context()); caller.UserID == u.ID {
		if _, err := s.checkDerivation(r.Context(), caller, auth.MintPassword); err != nil {
			s.derivationError(w, r, err)
			return
		}
	}
	if u.Status == identity.UserStatusDisabled {
		httpError(w, http.StatusUnprocessableEntity, "user %s is disabled — re-enable the account before resetting its password", u.Email)
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
	// The old credential (and whoever holds it) loses every session before
	// the new one lands, and again after it (auth.Service.ResetPasswordByAdmin):
	// a revocation that fails before the commit is answered as the failure it
	// is, and no temporary password is committed or emitted. The credential
	// and the rotation flag move together, under the precondition that makes
	// the read-time refusal above a guarantee: a deactivation that commits
	// inside this window takes the write, not the other way round, and no
	// temporary password is emitted. Into its own variable: a refused patch
	// answers the zero User, and the refusal below names the account from the
	// row that was read.
	patched, err := s.authSvc.ResetPasswordByAdmin(r.Context(), u.ID, hash)
	if errors.Is(err, identity.ErrUserStatusChanged) {
		httpError(w, http.StatusUnprocessableEntity, "user %s is disabled — re-enable the account before resetting its password", u.Email)
		return
	}
	if errors.Is(err, auth.ErrCredentialChangeBusy) {
		s.httpErrorCode(w, r, http.StatusConflict, "credential_change_busy", "%s", err.Error())
		return
	}
	if err != nil {
		if status := mapAuthErrorStatus(err); status < http.StatusInternalServerError {
			httpError(w, status, "%s", err.Error())
			return
		}
		s.identityFailure(w, r, fmt.Errorf("reset the password of user %s: %w", u.ID, err))
		return
	}
	u = patched
	s.auditPlatform(r, "", "user.password_reset", "user", u.ID, nil)
	writeJSON(w, struct {
		TempPassword string `json:"temp_password"`
	}{TempPassword: temp})
}

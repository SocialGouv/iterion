package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// LoginWithExternal completes an OIDC/OAuth flow. It either:
//   - finds an existing user via OIDCLink → logs them in,
//   - finds a user by email → links the new identity to them,
//   - in SignupOpen mode, creates a fresh user (+ personal team),
//   - in SignupInviteOnly mode, returns ErrSignupClosed unless the
//     user was already provisioned.
func (s *Service) LoginWithExternal(ctx context.Context, ext oidc.ExternalUser, userAgent, ip string) (LoginResult, error) {
	if ext.Subject == "" {
		return LoginResult{}, fmt.Errorf("auth: external user missing subject")
	}
	if ext.Email == "" {
		return LoginResult{}, oidc.ErrEmailMissing
	}
	now := s.now().UTC()

	link, err := s.store.GetOIDCLink(ctx, ext.Provider, ext.Subject)
	if err == nil {
		u, err := s.store.GetUser(ctx, link.UserID)
		if err != nil {
			return LoginResult{}, err
		}
		if u.Status == identity.UserStatusDisabled {
			return LoginResult{}, ErrAccountDisabled
		}
		u.LastLoginAt = &now
		_ = s.store.UpdateUser(ctx, u)
		return s.issueLogin(ctx, u, userAgent, ip)
	}
	if !errors.Is(err, identity.ErrNotFound) {
		return LoginResult{}, err
	}

	email := identity.NormalizeEmail(ext.Email)
	u, err := s.store.GetUserByEmail(ctx, email)
	if err == nil {
		if u.Status == identity.UserStatusDisabled {
			return LoginResult{}, ErrAccountDisabled
		}
		// Link the new external identity to the existing user.
		if err := s.store.UpsertOIDCLink(ctx, identity.OIDCLink{
			Provider:       ext.Provider,
			ProviderUserID: ext.Subject,
			UserID:         u.ID,
			Email:          email,
			CreatedAt:      now,
		}); err != nil {
			return LoginResult{}, err
		}
		u.LastLoginAt = &now
		_ = s.store.UpdateUser(ctx, u)
		return s.issueLogin(ctx, u, userAgent, ip)
	}
	if !errors.Is(err, identity.ErrNotFound) {
		return LoginResult{}, err
	}

	// New user via SSO.
	if s.signupMode != SignupOpen {
		return LoginResult{}, ErrSignupClosed
	}
	u = identity.User{
		ID:        uuid.NewString(),
		Email:     email,
		Name:      ext.Name,
		Status:    identity.UserStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	u, err = s.store.CreateUser(ctx, u)
	if err != nil {
		return LoginResult{}, err
	}
	teamID, err := s.createPersonalTeam(ctx, u)
	if err != nil {
		return LoginResult{}, err
	}
	u.DefaultTeamID = teamID
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return LoginResult{}, err
	}
	if err := s.store.UpsertOIDCLink(ctx, identity.OIDCLink{
		Provider:       ext.Provider,
		ProviderUserID: ext.Subject,
		UserID:         u.ID,
		Email:          email,
		CreatedAt:      now,
	}); err != nil {
		return LoginResult{}, err
	}
	u.LastLoginAt = &now
	_ = s.store.UpdateUser(ctx, u)
	return s.issueLogin(ctx, u, userAgent, ip)
}

// SwitchTeamWithCookie is identical to SwitchTeam but also returns
// the AccessTTL so the caller can stamp the cookie max-age in
// lock-step with the JWT.
func (s *Service) SwitchTeamWithCookie(ctx context.Context, userID, teamID string) (Identity, string, time.Time, error) {
	return s.SwitchTeam(ctx, userID, teamID)
}

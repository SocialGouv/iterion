package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/mail"
)

// Sentinel errors raised by Service. Handlers map them to HTTP
// statuses (Unauthorized, Forbidden, NotFound, Conflict).
var (
	ErrInvalidCredentials     = errors.New("auth: invalid credentials")
	ErrAccountDisabled        = errors.New("auth: account disabled")
	ErrPasswordChangeRequired = errors.New("auth: password change required")
	ErrPasswordWeak           = errors.New("auth: password too weak")
	ErrSignupClosed           = errors.New("auth: signup is invite-only")
	ErrLinkRequiresConsent    = errors.New("auth: SSO login matched an existing account by email — link explicitly from settings")
	ErrSSORestricted          = errors.New("auth: SSO login is restricted to allow-listed GitHub teams")
	ErrLinkAlreadyOwned       = errors.New("auth: this SSO identity is already linked to a different account")
	ErrInvitationNotFound     = errors.New("auth: invitation not found")
	ErrInvitationMismatch     = errors.New("auth: invitation does not match user")
	ErrTeamNotFound           = errors.New("auth: team not found")
	ErrNotAMember             = errors.New("auth: user is not a member of the team")
	ErrOrgNotFound            = errors.New("auth: org not found")
	ErrNotAnOrgMember         = errors.New("auth: user is not a member of the org")
	ErrNoTeamInOrg            = errors.New("auth: user has no team in the org")
)

// LockoutThreshold is the number of consecutive failed password
// attempts that triggers a temporary account lockout. The counter
// resets to zero on any successful Login. Tuned conservatively: high
// enough that a typo cluster doesn't lock a legitimate user, low
// enough that an attacker can't credential-stuff at full rate.
const LockoutThreshold = 5

// LockoutDuration is the wall-clock window a lockout lasts. While
// active, the gate at the top of Login short-circuits with
// ErrInvalidCredentials regardless of the password supplied — the
// constant-time dummy-hash check still runs so timing doesn't leak
// the lockout state.
const LockoutDuration = 15 * time.Minute

// SignupMode controls who may register without an invitation.
type SignupMode string

const (
	SignupOpen       SignupMode = "open"
	SignupInviteOnly SignupMode = "invite_only"
)

// GitHubUngrantedPolicy controls what happens to a GitHub SSO login that
// reaches the deployment while team-gating is active but matches no
// allow-listed team (orgsso.GitHubTeamGrant). It decouples GitHub admission
// from the global SignupMode.
type GitHubUngrantedPolicy string

const (
	// GitHubUngrantedRefuse rejects the login (ErrSSORestricted). Default —
	// preserves the historical behaviour.
	GitHubUngrantedRefuse GitHubUngrantedPolicy = "refuse"
	// GitHubUngrantedSubmitter admits the user as a teamless account (no
	// personal team), so they can authenticate + submit to the marketplace
	// but have no other rights until an admin adds them to an allow-listed
	// team. This is the public "sign up with GitHub to submit a bot" tier.
	GitHubUngrantedSubmitter GitHubUngrantedPolicy = "submitter"
)

// MinPasswordLen is the floor enforced at registration. Set
// intentionally low to avoid frustrating users with a password
// manager; argon2id covers the brute-force surface.
const MinPasswordLen = 8

// InvitationTTL is the default lifetime of an invitation. Keep
// generous (7 days) so users on email-light schedules still arrive
// in time.
const InvitationTTL = 7 * 24 * time.Hour

// Service is the high-level entry point for authentication and
// identity-mutation flows. Handlers in pkg/server depend on this
// type, not directly on the Mongo stores, so tests can swap in
// memory-backed implementations.
type Service struct {
	store      identity.Store
	sessions   SessionStore
	signer     *JWTSigner
	signupMode SignupMode
	// githubUngrantedPolicy decides whether a GitHub login matching no
	// allow-listed team is refused or admitted teamless (see B2 in
	// LoginWithExternal). Defaults to GitHubUngrantedRefuse.
	githubUngrantedPolicy GitHubUngrantedPolicy
	now                   func() time.Time
	refreshTTL            time.Duration
	// trustedAutoLinkProviders names the OIDC providers whose verified
	// email may be used to link a fresh external identity onto an
	// existing password-account user without an explicit "link this
	// connection" UI step. Empty = no auto-link (the default).
	// Operators only enroll providers here when they fully trust the
	// IdP's email verification (e.g. an in-house SSO they control).
	trustedAutoLinkProviders map[string]struct{}
	// orgSSO holds per-tenant SSO provider rows. nil disables every per-org
	// branch (LoginWithExternalForOrg → ErrUnknownProvider) and the GitHub
	// team-grant reverse lookup in LoginWithExternal becomes a no-op.
	orgSSO orgsso.Store
	// domains holds per-tenant verified email-domain claims, gating per-org
	// auto-link. nil → auto-link is never permitted (existing-email logins fall
	// back to ErrLinkRequiresConsent).
	domains orgsso.DomainStore
	// logger, when non-nil, receives audit-grade events like
	// "stored hash unparseable for user X" that we don't want to
	// silently swallow into a generic ErrInvalidCredentials response.
	logger *iterlog.Logger
	// resets + mailer + publicURL power the self-service password
	// reset (password_reset.go) and invitation emails. All optional:
	// nil resets/mailer disable the reset flow (request becomes a
	// logged no-op).
	resets    PasswordResetStore
	mailer    mail.Mailer
	publicURL string
}

// Config wires the Service.
type Config struct {
	Store      identity.Store
	Sessions   SessionStore
	Signer     *JWTSigner
	SignupMode SignupMode
	// GitHubUngrantedPolicy controls a gated GitHub login that matches no
	// allow-listed team: "refuse" (default) or "submitter" (teamless admit).
	GitHubUngrantedPolicy GitHubUngrantedPolicy
	RefreshTTL            time.Duration
	// TrustedAutoLinkProviders is the operator-configured allowlist of
	// OIDC providers whose verified email is safe to auto-link onto a
	// pre-existing password-account user. Empty = no auto-link; a fresh
	// SSO login that finds an existing user by email returns
	// ErrLinkRequiresConsent so the UI can prompt the user to link
	// manually from their settings.
	TrustedAutoLinkProviders []string
	// OrgSSO is the per-tenant SSO provider store (per-org Keycloak rows +
	// GitHub team-gating). Optional; nil disables per-org SSO.
	OrgSSO orgsso.Store
	// Domains is the per-tenant verified email-domain store gating per-org
	// auto-link. Optional; nil disables auto-link.
	Domains orgsso.DomainStore
	Logger  *iterlog.Logger
	// Resets + Mailer + PublicURL enable the password-reset flow and
	// invitation emails (all optional — see Service fields).
	Resets    PasswordResetStore
	Mailer    mail.Mailer
	PublicURL string
}

// NewService validates the config and returns a wired Service.
func NewService(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("auth: nil identity store")
	}
	if cfg.Sessions == nil {
		return nil, errors.New("auth: nil session store")
	}
	if cfg.Signer == nil {
		return nil, errors.New("auth: nil signer")
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = 30 * 24 * time.Hour
	}
	if cfg.SignupMode == "" {
		cfg.SignupMode = SignupInviteOnly
	}
	switch cfg.SignupMode {
	case SignupOpen, SignupInviteOnly:
	default:
		return nil, fmt.Errorf("auth: invalid signup mode %q", cfg.SignupMode)
	}
	if cfg.GitHubUngrantedPolicy == "" {
		cfg.GitHubUngrantedPolicy = GitHubUngrantedRefuse
	}
	switch cfg.GitHubUngrantedPolicy {
	case GitHubUngrantedRefuse, GitHubUngrantedSubmitter:
	default:
		return nil, fmt.Errorf("auth: invalid github ungranted policy %q", cfg.GitHubUngrantedPolicy)
	}
	trusted := make(map[string]struct{}, len(cfg.TrustedAutoLinkProviders))
	for _, p := range cfg.TrustedAutoLinkProviders {
		if p != "" {
			trusted[p] = struct{}{}
		}
	}
	return &Service{
		store:                    cfg.Store,
		sessions:                 cfg.Sessions,
		signer:                   cfg.Signer,
		signupMode:               cfg.SignupMode,
		githubUngrantedPolicy:    cfg.GitHubUngrantedPolicy,
		refreshTTL:               cfg.RefreshTTL,
		now:                      time.Now,
		trustedAutoLinkProviders: trusted,
		orgSSO:                   cfg.OrgSSO,
		domains:                  cfg.Domains,
		logger:                   cfg.Logger,
		resets:                   cfg.Resets,
		mailer:                   cfg.Mailer,
		publicURL:                strings.TrimRight(cfg.PublicURL, "/"),
	}, nil
}

// EmailEnabled reports whether a real mailer is wired (drives the
// SPA's forgot-password entry point via server_info).
func (s *Service) EmailEnabled() bool { return s.mailer != nil && s.mailer.Enabled() }

// Mailer exposes the wired mailer (nil-safe for callers that send
// optional notifications like invitation emails).
func (s *Service) Mailer() mail.Mailer { return s.mailer }

// PublicURL is the externally-reachable base URL used in email links.
func (s *Service) PublicURL() string { return s.publicURL }

// LoginResult bundles the artifacts returned to the caller after a
// successful login or refresh. The HTTP layer translates these into
// cookies / JSON.
type LoginResult struct {
	User           identity.User
	ActiveOrgID    string
	ActiveOrgRole  identity.OrgRole
	ActiveTeamID   string
	ActiveRole     identity.Role
	AccessToken    string
	AccessExpires  time.Time
	RefreshToken   string
	RefreshExpires time.Time
	Memberships    []identity.Membership
	OrgMemberships []identity.OrgMembership
}

// Login authenticates with email + password. On success, issues an
// access JWT bound to the user's default team (or first available)
// and a refresh token.
func (s *Service) Login(ctx context.Context, email, password, userAgent, ip string) (LoginResult, error) {
	u, err := s.store.GetUserByEmail(ctx, email)
	if errors.Is(err, identity.ErrNotFound) {
		// Spend the same time hashing a dummy password so we don't
		// leak account existence via response timing.
		_, _ = VerifyPassword(password, dummyHash)
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, err
	}
	if u.Status == identity.UserStatusDisabled {
		return LoginResult{}, ErrAccountDisabled
	}
	if u.LockedUntil != nil && s.now().Before(*u.LockedUntil) {
		// Spend the same argon2id cycles a real attempt would, so a
		// network observer can't distinguish "locked" from "wrong
		// password" by response timing. The LockoutDuration docstring
		// promises this; the gate must honour it.
		_, _ = VerifyPassword(password, dummyHash)
		return LoginResult{}, ErrInvalidCredentials
	}
	ok, err := VerifyPassword(password, u.PasswordHash)
	if err != nil {
		// A non-nil error from VerifyPassword is structural (corrupt
		// stored hash, unknown algorithm prefix) — distinct from a
		// plain mismatch. Swallowing it as ErrInvalidCredentials
		// leaves the account permanently un-loginable with no signal
		// for the operator. Log the failure under the user id so it
		// surfaces in audit aggregation without leaking the hash.
		if s.logger != nil {
			s.logger.Error("auth: stored password hash for user %s is unparseable: %v", u.ID, err)
		}
		return LoginResult{}, ErrInvalidCredentials
	}
	if !ok {
		// Brute-force lockout: increment the failed counter and lock
		// the account when it reaches LockoutThreshold. The gate at
		// the top of Login then short-circuits subsequent attempts
		// during LockoutDuration. The counter persistence is
		// best-effort — a write failure logs at error but doesn't
		// surface to the caller, since the response shape must remain
		// indistinguishable from a non-locked invalid-credentials
		// case (per the comment on dummyHash above).
		u.FailedLogins++
		if u.FailedLogins >= LockoutThreshold {
			lockUntil := s.now().UTC().Add(LockoutDuration)
			u.LockedUntil = &lockUntil
		}
		if persistErr := s.store.UpdateUser(ctx, u); persistErr != nil && s.logger != nil {
			s.logger.Error("auth: persist failed-login counter for %s: %v", u.ID, persistErr)
		}
		return LoginResult{}, ErrInvalidCredentials
	}

	// Password verified — but a pending_password_change status must
	// not yield a fully-privileged session. Returning the dedicated
	// sentinel lets the HTTP layer / SPA route to a change-password
	// flow without first minting access + refresh tokens.
	if u.Status == identity.UserStatusPendingPasswordChange {
		return LoginResult{}, ErrPasswordChangeRequired
	}

	now := s.now().UTC()
	u.LastLoginAt = &now
	u.FailedLogins = 0
	u.LockedUntil = nil
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return LoginResult{}, err
	}

	return s.issueLogin(ctx, u, userAgent, ip)
}

// ChangePasswordPending completes the forced-rotation flow for a user in
// pending_password_change status (e.g. the bootstrapped super-admin): it
// verifies the temporary password, sets the chosen new one, marks the
// account active, and issues a normal session. It applies ONLY to
// pending_password_change accounts — rotating an already-active account's
// password is a separate, authenticated concern — and returns the opaque
// ErrInvalidCredentials for a missing user, wrong status, or bad temp
// password so the endpoint cannot enumerate accounts or their state.
func (s *Service) ChangePasswordPending(ctx context.Context, email, currentPassword, newPassword, userAgent, ip string) (LoginResult, error) {
	if len(newPassword) < MinPasswordLen {
		return LoginResult{}, ErrPasswordWeak
	}
	u, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		// Spend the same argon2id cycles a real attempt would so a
		// network observer can't distinguish "no such account" from
		// "wrong temp password" by response timing (matches Login).
		_, _ = VerifyPassword(currentPassword, dummyHash)
		return LoginResult{}, ErrInvalidCredentials
	}
	if u.Status != identity.UserStatusPendingPasswordChange {
		// Same timing-equalisation for the wrong-status branch.
		_, _ = VerifyPassword(currentPassword, dummyHash)
		return LoginResult{}, ErrInvalidCredentials
	}
	ok, err := VerifyPassword(currentPassword, u.PasswordHash)
	if err != nil || !ok {
		return LoginResult{}, ErrInvalidCredentials
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return LoginResult{}, err
	}
	now := s.now().UTC()
	u.PasswordHash = hash
	u.Status = identity.UserStatusActive
	u.FailedLogins = 0
	u.LockedUntil = nil
	u.LastLoginAt = &now
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return LoginResult{}, err
	}
	return s.issueLogin(ctx, u, userAgent, ip)
}

// issueLogin is the shared post-authentication path used by Login
// and AcceptInvitation. It picks the active team, mints tokens, and
// records the refresh session.
func (s *Service) issueLogin(ctx context.Context, u identity.User, userAgent, ip string) (LoginResult, error) {
	return s.issueLoginInTeam(ctx, u, "", userAgent, ip)
}

// issueLoginInTeam is issueLogin with a preferred active team: when
// preferredTeamID is one of the user's memberships, the JWT is stamped with it
// (and its role) instead of the DefaultTeamID/first-membership pick. Used by
// per-org SSO so a user who logs in via an org's own Keycloak lands in that
// org rather than their stored default.
func (s *Service) issueLoginInTeam(ctx context.Context, u identity.User, preferredTeamID, userAgent, ip string) (LoginResult, error) {
	memberships, err := s.store.ListMembershipsByUser(ctx, u.ID)
	if err != nil {
		return LoginResult{}, err
	}
	teamID, role := s.pickActiveTeam(u, memberships)
	if preferredTeamID != "" {
		for _, m := range memberships {
			if m.TeamID == preferredTeamID {
				teamID, role = m.TeamID, m.Role
				break
			}
		}
	}
	orgID, orgRole := s.resolveOrgForTeam(ctx, u, teamID)
	orgMems, err := s.store.ListOrgMembershipsByUser(ctx, u.ID)
	if err != nil {
		return LoginResult{}, err
	}
	id := Identity{
		UserID:       u.ID,
		Email:        u.Email,
		OrgID:        orgID,
		OrgRole:      orgRole,
		TeamID:       teamID,
		Role:         role,
		IsSuperAdmin: u.IsSuperAdmin,
	}
	access, exp, err := s.signer.IssueAccess(id)
	if err != nil {
		return LoginResult{}, err
	}
	refresh, sess, err := IssueSession(ctx, s.sessions, u.ID, userAgent, ip, s.refreshTTL)
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{
		User:           u,
		ActiveOrgID:    orgID,
		ActiveOrgRole:  orgRole,
		ActiveTeamID:   teamID,
		ActiveRole:     role,
		AccessToken:    access,
		AccessExpires:  exp,
		RefreshToken:   refresh,
		RefreshExpires: sess.ExpiresAt,
		Memberships:    memberships,
		OrgMemberships: orgMems,
	}, nil
}

// resolveOrgForTeam derives the active org context for a chosen team:
// the team's parent OrgID and the user's role in that org. A super-admin
// with no org-membership row is treated as an org admin (mirrors the
// team-level super-admin step-in). Returns empty strings when teamID is
// empty or the team has no parent org yet (pre-backfill row).
func (s *Service) resolveOrgForTeam(ctx context.Context, u identity.User, teamID string) (orgID string, orgRole identity.OrgRole) {
	if teamID == "" {
		return "", ""
	}
	t, err := s.store.GetTeam(ctx, teamID)
	if err != nil || t.OrgID == "" {
		return "", ""
	}
	om, err := s.store.GetOrgMembership(ctx, u.ID, t.OrgID)
	if err == nil {
		return t.OrgID, om.Role
	}
	if u.IsSuperAdmin {
		return t.OrgID, identity.OrgRoleAdmin
	}
	return t.OrgID, ""
}

// pickActiveTeam picks the JWT-stamped team based on (1) the user's
// stored DefaultTeamID, (2) the first team they're a member of, or
// (3) empty (super-admin without memberships — UI lands them on
// /admin/teams to pick one).
func (s *Service) pickActiveTeam(u identity.User, ms []identity.Membership) (string, identity.Role) {
	if u.DefaultTeamID != "" {
		for _, m := range ms {
			if m.TeamID == u.DefaultTeamID {
				return m.TeamID, m.Role
			}
		}
	}
	if len(ms) > 0 {
		return ms[0].TeamID, ms[0].Role
	}
	return "", ""
}

// Refresh rotates a presented refresh token. Returns a LoginResult
// the caller can re-set the cookies / response with.
func (s *Service) Refresh(ctx context.Context, presented, userAgent, ip string) (LoginResult, error) {
	hash := HashRefreshToken(presented)
	prev, err := s.sessions.GetSessionByTokenHash(ctx, hash)
	if err != nil {
		return LoginResult{}, err
	}
	now := s.now().UTC()
	if prev.RevokedAt != nil {
		// Token reuse is suspicious: revoke all sessions of this user.
		// A failed revoke leaves the (possibly stolen) siblings live, so
		// log it rather than swallow it — this is a security event.
		if err := s.sessions.RevokeUserSessions(ctx, prev.UserID, now); err != nil && s.logger != nil {
			s.logger.Error("auth: failed to revoke sessions after refresh-token reuse for user %s: %v", prev.UserID, err)
		}
		return LoginResult{}, ErrSessionRevoked
	}
	if !prev.ExpiresAt.IsZero() && now.After(prev.ExpiresAt) {
		return LoginResult{}, ErrSessionExpired
	}
	u, err := s.store.GetUser(ctx, prev.UserID)
	if err != nil {
		return LoginResult{}, err
	}
	if u.Status == identity.UserStatusDisabled {
		// Revoke the presented session so a disabled account doesn't
		// leave a refresh-token slot alive until refreshTTL expires.
		// Re-enabling the user later should not resurrect old sessions.
		_, _ = s.sessions.RevokeSessionIfNotRevoked(ctx, prev.ID, now)
		return LoginResult{}, ErrAccountDisabled
	}
	// Rotate. CAS-revoke prevents two parallel refresh calls from
	// both passing the "not yet revoked" check above and both
	// proceeding to issueLogin — without it the same refresh token
	// could mint two access tokens that outlive each other.
	revoked, err := s.sessions.RevokeSessionIfNotRevoked(ctx, prev.ID, now)
	if err != nil {
		return LoginResult{}, err
	}
	if !revoked {
		// A concurrent Refresh already rotated this session. Treat as
		// reuse: revoke every session of the user and surface the
		// stronger error so the SPA forces a clean re-login. A failed
		// revoke leaves the (possibly stolen) siblings live — log it
		// rather than swallow it.
		if err := s.sessions.RevokeUserSessions(ctx, prev.UserID, now); err != nil && s.logger != nil {
			s.logger.Error("auth: failed to revoke sessions after refresh-token reuse for user %s: %v", prev.UserID, err)
		}
		return LoginResult{}, ErrSessionRevoked
	}
	return s.issueLogin(ctx, u, userAgent, ip)
}

// RevokeUserSessions invalidates every live refresh session for the
// user. Used by the admin "disable user" flow so the user loses
// access at the next access-token expiry (≤15 min) instead of waiting
// for refresh TTL (~30 days). Best-effort: a store-write failure is
// surfaced to the caller, which typically logs and continues.
func (s *Service) RevokeUserSessions(ctx context.Context, userID string) error {
	return s.sessions.RevokeUserSessions(ctx, userID, s.now().UTC())
}

// Logout revokes the presented refresh token only. Other devices
// (other refresh tokens for the same user) keep their sessions.
func (s *Service) Logout(ctx context.Context, presented string) error {
	hash := HashRefreshToken(presented)
	sess, err := s.sessions.GetSessionByTokenHash(ctx, hash)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return nil
		}
		return err
	}
	return s.sessions.RevokeSession(ctx, sess.ID, s.now().UTC())
}

// SwitchTeam re-issues the access JWT bound to teamID. Validates that
// the current user is a member of the team — and, transitively, of its
// parent org (you cannot hold a team grant without an org membership,
// so the team check is sufficient; super-admins step into either).
func (s *Service) SwitchTeam(ctx context.Context, userID, teamID string) (Identity, string, time.Time, error) {
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return Identity{}, "", time.Time{}, err
	}
	mb, err := s.store.GetMembership(ctx, userID, teamID)
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			if !u.IsSuperAdmin {
				return Identity{}, "", time.Time{}, ErrNotAMember
			}
			// Super-admins can step into any team without a membership row.
			team, terr := s.store.GetTeam(ctx, teamID)
			if terr != nil {
				return Identity{}, "", time.Time{}, ErrTeamNotFound
			}
			mb = identity.Membership{UserID: userID, TeamID: team.ID, Role: identity.RoleAdmin}
		} else {
			return Identity{}, "", time.Time{}, err
		}
	}
	orgID, orgRole := s.resolveOrgForTeam(ctx, u, mb.TeamID)
	id := Identity{
		UserID:       u.ID,
		Email:        u.Email,
		OrgID:        orgID,
		OrgRole:      orgRole,
		TeamID:       mb.TeamID,
		Role:         mb.Role,
		IsSuperAdmin: u.IsSuperAdmin,
	}
	tok, exp, err := s.signer.IssueAccess(id)
	if err != nil {
		return Identity{}, "", time.Time{}, err
	}
	// Persist the choice as the user's new default (team + org). The JWT
	// is already issued and carries the chosen context — a persistence
	// failure here means the next session won't auto-resume, but the
	// current login is still good. Log so an operator can investigate.
	u.DefaultTeamID = teamID
	if orgID != "" {
		u.DefaultOrgID = orgID
	}
	if uerr := s.store.UpdateUser(ctx, u); uerr != nil && s.logger != nil {
		s.logger.Warn("auth: persist default team for user %s failed: %v", u.ID, uerr)
	}
	return id, tok, exp, nil
}

// SwitchOrg re-issues the access JWT bound to a different org. It picks
// an active team within that org (the user's default team if it belongs
// to the org, else the first team they're granted there) so the session
// always lands on a concrete workspace. A user with no team in the org
// still switches — they land org-scoped with an empty active team (the
// UI offers to create one). Validates org membership (super-admins step
// into any org).
func (s *Service) SwitchOrg(ctx context.Context, userID, orgID string) (Identity, string, time.Time, error) {
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return Identity{}, "", time.Time{}, err
	}
	om, err := s.store.GetOrgMembership(ctx, userID, orgID)
	orgRole := om.Role
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			if !u.IsSuperAdmin {
				return Identity{}, "", time.Time{}, ErrNotAnOrgMember
			}
			if _, oerr := s.store.GetOrg(ctx, orgID); oerr != nil {
				return Identity{}, "", time.Time{}, ErrOrgNotFound
			}
			orgRole = identity.OrgRoleAdmin
		} else {
			return Identity{}, "", time.Time{}, err
		}
	}
	// Pick a team in the org the user is granted (or any team for a
	// super-admin), preferring their stored default.
	teamID, teamRole := s.pickActiveTeamInOrg(ctx, u, orgID)
	id := Identity{
		UserID:       u.ID,
		Email:        u.Email,
		OrgID:        orgID,
		OrgRole:      orgRole,
		TeamID:       teamID,
		Role:         teamRole,
		IsSuperAdmin: u.IsSuperAdmin,
	}
	tok, exp, err := s.signer.IssueAccess(id)
	if err != nil {
		return Identity{}, "", time.Time{}, err
	}
	u.DefaultOrgID = orgID
	if teamID != "" {
		u.DefaultTeamID = teamID
	}
	if uerr := s.store.UpdateUser(ctx, u); uerr != nil && s.logger != nil {
		s.logger.Warn("auth: persist default org for user %s failed: %v", u.ID, uerr)
	}
	return id, tok, exp, nil
}

// pickActiveTeamInOrg selects a team for the user within one org: their
// stored DefaultTeamID if it belongs to the org and they're a member,
// else the first team they're granted in the org. Returns ("","") when
// the user holds no team in the org (super-admins fall back to the org's
// first team so they always land somewhere).
func (s *Service) pickActiveTeamInOrg(ctx context.Context, u identity.User, orgID string) (string, identity.Role) {
	memberships, err := s.store.ListMembershipsByUser(ctx, u.ID)
	if err != nil {
		return "", ""
	}
	// One read for the org's teams, then intersect with the user's grants —
	// instead of a GetTeam per membership.
	teams, terr := s.store.ListTeamsByOrg(ctx, orgID)
	if terr != nil {
		return "", ""
	}
	inOrg := make(map[string]bool, len(teams))
	for _, t := range teams {
		inOrg[t.ID] = true
	}
	var first *identity.Membership
	for i := range memberships {
		if !inOrg[memberships[i].TeamID] {
			continue
		}
		if memberships[i].TeamID == u.DefaultTeamID {
			return memberships[i].TeamID, memberships[i].Role
		}
		if first == nil {
			first = &memberships[i]
		}
	}
	if first != nil {
		return first.TeamID, first.Role
	}
	if u.IsSuperAdmin && len(teams) > 0 {
		return teams[0].ID, identity.RoleAdmin
	}
	return "", ""
}

// Register creates a new user. When SignupMode is invite_only, an
// invitation token must be supplied and matching the email.
func (s *Service) Register(ctx context.Context, email, password, name, invitationToken, userAgent, ip string) (LoginResult, error) {
	email = identity.NormalizeEmail(email)
	if email == "" {
		return LoginResult{}, ErrInvalidCredentials
	}
	if len(password) < MinPasswordLen {
		return LoginResult{}, ErrPasswordWeak
	}
	switch s.signupMode {
	case SignupOpen:
		return s.registerOpen(ctx, email, password, name, userAgent, ip)
	case SignupInviteOnly:
		if invitationToken == "" {
			return LoginResult{}, ErrSignupClosed
		}
		return s.registerWithInvitation(ctx, email, password, name, invitationToken, userAgent, ip)
	}
	return LoginResult{}, ErrSignupClosed
}

func (s *Service) registerOpen(ctx context.Context, email, password, name, userAgent, ip string) (LoginResult, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return LoginResult{}, err
	}
	now := s.now().UTC()
	u := identity.User{
		ID:           uuid.NewString(),
		Email:        email,
		Name:         name,
		PasswordHash: hash,
		Status:       identity.UserStatusActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if u, err = s.store.CreateUser(ctx, u); err != nil {
		return LoginResult{}, err
	}
	// Auto-create a personal org + team so the user has somewhere to land.
	orgID, teamID, err := s.createPersonalTeam(ctx, u)
	if err != nil {
		return LoginResult{}, err
	}
	u.DefaultOrgID = orgID
	u.DefaultTeamID = teamID
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return LoginResult{}, err
	}
	return s.issueLogin(ctx, u, userAgent, ip)
}

func (s *Service) registerWithInvitation(ctx context.Context, email, password, name, token, userAgent, ip string) (LoginResult, error) {
	inv, err := s.store.GetInvitationByTokenHash(ctx, hashInvitationToken(token))
	if errors.Is(err, identity.ErrNotFound) {
		return LoginResult{}, ErrInvitationNotFound
	}
	if err != nil {
		return LoginResult{}, err
	}
	if inv.AcceptedAt != nil {
		return LoginResult{}, identity.ErrInvitationUsed
	}
	if !inv.ExpiresAt.IsZero() && s.now().After(inv.ExpiresAt) {
		return LoginResult{}, identity.ErrInvitationExpired
	}
	if identity.NormalizeEmail(inv.Email) != email {
		return LoginResult{}, ErrInvitationMismatch
	}
	hash, err := HashPassword(password)
	if err != nil {
		return LoginResult{}, err
	}
	now := s.now().UTC()
	u := identity.User{
		ID:           uuid.NewString(),
		Email:        email,
		Name:         name,
		PasswordHash: hash,
		Status:       identity.UserStatusActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if u, err = s.store.CreateUser(ctx, u); err != nil {
		return LoginResult{}, err
	}
	if err := s.store.UpsertMembership(ctx, identity.Membership{
		UserID:    u.ID,
		TeamID:    inv.TeamID,
		Role:      inv.Role,
		InvitedBy: inv.InvitedBy,
		JoinedAt:  now,
	}); err != nil {
		return LoginResult{}, err
	}
	// Mirror the team grant up to an org membership so the user is a
	// first-class member of the team's parent org.
	orgID, err := s.grantOrgMembershipForTeam(ctx, u.ID, inv.TeamID, inv.Role, now)
	if err != nil {
		return LoginResult{}, err
	}
	u.DefaultTeamID = inv.TeamID
	if orgID != "" {
		u.DefaultOrgID = orgID
	}
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return LoginResult{}, err
	}
	acceptedAt := now
	inv.AcceptedAt = &acceptedAt
	inv.AcceptedBy = u.ID
	if err := s.store.UpdateInvitation(ctx, inv); err != nil {
		return LoginResult{}, err
	}
	return s.issueLogin(ctx, u, userAgent, ip)
}

// grantOrgMembershipForTeam ensures the user holds an org membership in
// the parent org of teamID, with at least the org-role implied by their
// team role (never downgrades an existing higher org-role). Returns the
// resolved orgID (empty if the team has no parent org yet).
func (s *Service) grantOrgMembershipForTeam(ctx context.Context, userID, teamID string, teamRole identity.Role, now time.Time) (string, error) {
	t, err := s.store.GetTeam(ctx, teamID)
	if err != nil {
		return "", err
	}
	if t.OrgID == "" {
		return "", nil
	}
	want := identity.OrgRoleForTeamRole(teamRole)
	if want == "" {
		// A team capability outside the ladder (config_editor) confers no org
		// membership — never mirror it up (an org-member row would pass
		// canViewOrg → org roster/usage read). Login still resolves the org
		// context with an empty OrgRole, which the org gates reject. ADR-078.
		return t.OrgID, nil
	}
	// Never downgrade an existing higher org-role.
	if existing, gerr := s.store.GetOrgMembership(ctx, userID, t.OrgID); gerr == nil && existing.Role.AtLeast(want) {
		return t.OrgID, nil
	}
	if err := s.store.UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID:   userID,
		OrgID:    t.OrgID,
		Role:     want,
		JoinedAt: now,
	}); err != nil {
		return "", err
	}
	return t.OrgID, nil
}

// AcceptInvitationForExistingUser is the path used when an invited
// email already corresponds to a registered user — they accept by
// adding a membership.
func (s *Service) AcceptInvitationForExistingUser(ctx context.Context, userID, token string) (identity.Membership, error) {
	inv, err := s.store.GetInvitationByTokenHash(ctx, hashInvitationToken(token))
	if errors.Is(err, identity.ErrNotFound) {
		return identity.Membership{}, ErrInvitationNotFound
	}
	if err != nil {
		return identity.Membership{}, err
	}
	if inv.AcceptedAt != nil {
		return identity.Membership{}, identity.ErrInvitationUsed
	}
	if !inv.ExpiresAt.IsZero() && s.now().After(inv.ExpiresAt) {
		return identity.Membership{}, identity.ErrInvitationExpired
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return identity.Membership{}, err
	}
	if identity.NormalizeEmail(inv.Email) != u.Email {
		return identity.Membership{}, ErrInvitationMismatch
	}
	now := s.now().UTC()
	mb := identity.Membership{
		UserID:    u.ID,
		TeamID:    inv.TeamID,
		Role:      inv.Role,
		InvitedBy: inv.InvitedBy,
		JoinedAt:  now,
	}
	if err := s.store.UpsertMembership(ctx, mb); err != nil {
		return identity.Membership{}, err
	}
	if _, err := s.grantOrgMembershipForTeam(ctx, u.ID, inv.TeamID, inv.Role, now); err != nil {
		return identity.Membership{}, err
	}
	acceptedAt := now
	inv.AcceptedAt = &acceptedAt
	inv.AcceptedBy = u.ID
	if err := s.store.UpdateInvitation(ctx, inv); err != nil {
		return identity.Membership{}, err
	}
	return mb, nil
}

// Store returns the underlying identity store. Server handlers
// reach for it to perform read-only joins (e.g. resolving team
// names from membership rows) without proxying every accessor
// through the Service.
func (s *Service) Store() identity.Store { return s.store }

// CreateTeamFor provisions a non-personal team inside org `orgID`,
// owned by user `userID`. The user must be a member of the org (super-
// admins may create in any org). Returns the new team. If slug is empty
// it is derived from name and uniquified with a numeric suffix on
// collision.
func (s *Service) CreateTeamFor(ctx context.Context, userID, orgID, name, slug string) (identity.Team, error) {
	if name == "" {
		return identity.Team{}, fmt.Errorf("auth: team name required")
	}
	if orgID == "" {
		return identity.Team{}, fmt.Errorf("auth: org id required")
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return identity.Team{}, err
	}
	if _, err := s.store.GetOrg(ctx, orgID); err != nil {
		return identity.Team{}, ErrOrgNotFound
	}
	if _, err := s.store.GetOrgMembership(ctx, userID, orgID); err != nil {
		if errors.Is(err, identity.ErrNotFound) && !u.IsSuperAdmin {
			return identity.Team{}, ErrNotAnOrgMember
		}
		if !errors.Is(err, identity.ErrNotFound) {
			return identity.Team{}, err
		}
	}
	base := slug
	if base == "" {
		base = identity.SlugifyTeamName(name)
		if base == "" {
			base = "team"
		}
	}
	now := s.now().UTC()
	for attempt := 0; attempt < 10; attempt++ {
		try := base
		if attempt > 0 {
			try = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		t := identity.Team{
			ID:        uuid.NewString(),
			OrgID:     orgID,
			Name:      name,
			Slug:      try,
			CreatedBy: u.ID,
			CreatedAt: now,
			UpdatedAt: now,
		}
		_, err := s.store.CreateTeam(ctx, t)
		if errors.Is(err, identity.ErrSlugAlreadyTaken) {
			if slug != "" {
				return identity.Team{}, identity.ErrSlugAlreadyTaken
			}
			continue
		}
		if err != nil {
			return identity.Team{}, err
		}
		if err := s.store.UpsertMembership(ctx, identity.Membership{
			UserID:   u.ID,
			TeamID:   t.ID,
			Role:     identity.RoleOwner,
			JoinedAt: now,
		}); err != nil {
			return identity.Team{}, err
		}
		return t, nil
	}
	return identity.Team{}, errors.New("auth: could not allocate slug for team")
}

// CreateOrgFor provisions a new (non-personal) org owned by user
// `userID`, with an owner org-membership and a default team inside it.
// Returns the new org. Used by the super-admin org console and org
// self-service "create organization" flows. If slug is empty it is
// derived from name and uniquified with a numeric suffix on collision.
func (s *Service) CreateOrgFor(ctx context.Context, userID, name, slug string) (identity.Org, error) {
	if name == "" {
		return identity.Org{}, fmt.Errorf("auth: org name required")
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return identity.Org{}, err
	}
	base := slug
	if base == "" {
		base = identity.SlugifyTeamName(name)
		if base == "" {
			base = "org"
		}
	}
	now := s.now().UTC()
	for attempt := 0; attempt < 10; attempt++ {
		try := base
		if attempt > 0 {
			try = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		o := identity.Org{
			ID:        uuid.NewString(),
			Name:      name,
			Slug:      try,
			CreatedBy: u.ID,
			CreatedAt: now,
			UpdatedAt: now,
		}
		_, err := s.store.CreateOrg(ctx, o)
		if errors.Is(err, identity.ErrOrgSlugAlreadyTaken) {
			if slug != "" {
				return identity.Org{}, identity.ErrOrgSlugAlreadyTaken
			}
			continue
		}
		if err != nil {
			return identity.Org{}, err
		}
		if err := s.store.UpsertOrgMembership(ctx, identity.OrgMembership{
			UserID:   u.ID,
			OrgID:    o.ID,
			Role:     identity.OrgRoleOwner,
			JoinedAt: now,
		}); err != nil {
			return identity.Org{}, err
		}
		// Seed a default team so the org is immediately usable.
		if _, err := s.CreateTeamFor(ctx, u.ID, o.ID, name, ""); err != nil {
			return identity.Org{}, err
		}
		return o, nil
	}
	return identity.Org{}, errors.New("auth: could not allocate slug for org")
}

// DeleteOrgCascade removes an org and all its identity-scoped children:
// every team in the org (each team's memberships + pending invitations,
// then the team record), then all org memberships, then the org itself.
//
// It deliberately does NOT purge team-scoped resources living in OTHER
// stores (runs, board, forge connections, SSO providers, secrets) — those
// become orphaned and unreachable rather than deleted. Intended for
// super-admin cleanup of empty / migrated orgs; the caller (admin route)
// gates on super-admin and refuses to delete the caller's active org.
func (s *Service) DeleteOrgCascade(ctx context.Context, orgID string) error {
	if orgID == "" {
		return fmt.Errorf("auth: org id required")
	}
	if _, err := s.store.GetOrg(ctx, orgID); err != nil {
		return err
	}
	teams, err := s.store.ListTeamsByOrg(ctx, orgID)
	if err != nil {
		return fmt.Errorf("auth: list teams for org %s: %w", orgID, err)
	}
	for _, t := range teams {
		mems, err := s.store.ListMembershipsByTeam(ctx, t.ID)
		if err != nil {
			return fmt.Errorf("auth: list memberships for team %s: %w", t.ID, err)
		}
		for _, m := range mems {
			if err := s.store.DeleteMembership(ctx, m.UserID, t.ID); err != nil {
				return fmt.Errorf("auth: delete membership %s/%s: %w", m.UserID, t.ID, err)
			}
		}
		// Best-effort: drop the team's pending invitations.
		if invs, err := s.store.ListInvitationsByTeam(ctx, t.ID); err == nil {
			for _, inv := range invs {
				_ = s.store.DeleteInvitation(ctx, inv.ID)
			}
		}
		if err := s.store.DeleteTeam(ctx, t.ID); err != nil {
			return fmt.Errorf("auth: delete team %s: %w", t.ID, err)
		}
	}
	oms, err := s.store.ListOrgMembershipsByOrg(ctx, orgID)
	if err != nil {
		return fmt.Errorf("auth: list org memberships for org %s: %w", orgID, err)
	}
	for _, m := range oms {
		if err := s.store.DeleteOrgMembership(ctx, m.UserID, orgID); err != nil {
			return fmt.Errorf("auth: delete org membership %s/%s: %w", m.UserID, orgID, err)
		}
	}
	if err := s.store.DeleteOrg(ctx, orgID); err != nil {
		return fmt.Errorf("auth: delete org %s: %w", orgID, err)
	}
	return nil
}

// MarkOrgForDeletion soft-deletes an org: Status=pending_deletion and
// PurgeAfter=now+grace. The org is blocked immediately (Suspended()==true);
// the nightly sweeper hard-purges it once PurgeAfter passes. Restorable until
// then via RestoreOrg.
func (s *Service) MarkOrgForDeletion(ctx context.Context, orgID string, grace time.Duration) (identity.Org, error) {
	if orgID == "" {
		return identity.Org{}, fmt.Errorf("auth: org id required")
	}
	o, err := s.store.GetOrg(ctx, orgID)
	if err != nil {
		return identity.Org{}, err
	}
	now := time.Now().UTC()
	purge := now.Add(grace)
	o.Status = identity.TeamStatusPendingDeletion
	o.PurgeAfter = &purge
	o.UpdatedAt = now
	if err := s.store.UpdateOrg(ctx, o); err != nil {
		return identity.Org{}, fmt.Errorf("auth: mark org for deletion: %w", err)
	}
	return o, nil
}

// RestoreOrg cancels a pending deletion, returning the org to active. It is a
// no-op (returns the org unchanged) when the org isn't pending deletion.
func (s *Service) RestoreOrg(ctx context.Context, orgID string) (identity.Org, error) {
	o, err := s.store.GetOrg(ctx, orgID)
	if err != nil {
		return identity.Org{}, err
	}
	if o.EffectiveStatus() != identity.TeamStatusPendingDeletion {
		return o, nil
	}
	o.Status = identity.TeamStatusActive
	o.PurgeAfter = nil
	o.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateOrg(ctx, o); err != nil {
		return identity.Org{}, fmt.Errorf("auth: restore org: %w", err)
	}
	return o, nil
}

// CreateInvitation issues a fresh invitation. The plaintext token is
// returned (caller emails it) and only its hash is persisted.
func (s *Service) CreateInvitation(ctx context.Context, teamID, email string, role identity.Role, invitedBy string) (token string, inv identity.Invitation, err error) {
	if !role.Valid() {
		return "", identity.Invitation{}, identity.ErrInvalidRole
	}
	if _, err := s.store.GetTeam(ctx, teamID); err != nil {
		return "", identity.Invitation{}, ErrTeamNotFound
	}
	tok, _, err := GenerateRandomToken(32)
	if err != nil {
		return "", identity.Invitation{}, err
	}
	now := s.now().UTC()
	inv = identity.Invitation{
		ID:        uuid.NewString(),
		TeamID:    teamID,
		Email:     identity.NormalizeEmail(email),
		Role:      role,
		TokenHash: hashInvitationToken(tok),
		InvitedBy: invitedBy,
		CreatedAt: now,
		ExpiresAt: now.Add(InvitationTTL),
	}
	if err := s.store.CreateInvitation(ctx, inv); err != nil {
		return "", identity.Invitation{}, err
	}
	return tok, inv, nil
}

// CreateUserAndPersonalTeam is used by the bootstrap path to provision
// the very first super-admin. Idempotent: returns the existing user
// if email is already taken.
func (s *Service) CreateUserAndPersonalTeam(ctx context.Context, email, name, password string, isSuperAdmin bool, status identity.UserStatus) (identity.User, identity.Team, error) {
	email = identity.NormalizeEmail(email)
	if existing, err := s.store.GetUserByEmail(ctx, email); err == nil {
		// Find their personal team if any, else return zero team.
		ms, _ := s.store.ListMembershipsByUser(ctx, existing.ID)
		for _, mb := range ms {
			t, terr := s.store.GetTeam(ctx, mb.TeamID)
			if terr == nil && t.Personal {
				return existing, t, nil
			}
		}
		return existing, identity.Team{}, nil
	}
	hash, err := HashPassword(password)
	if err != nil {
		return identity.User{}, identity.Team{}, err
	}
	if status == "" {
		status = identity.UserStatusActive
	}
	now := s.now().UTC()
	u := identity.User{
		ID:           uuid.NewString(),
		Email:        email,
		Name:         name,
		PasswordHash: hash,
		Status:       status,
		IsSuperAdmin: isSuperAdmin,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	u, err = s.store.CreateUser(ctx, u)
	if err != nil {
		return identity.User{}, identity.Team{}, err
	}
	orgID, teamID, err := s.createPersonalTeam(ctx, u)
	if err != nil {
		return identity.User{}, identity.Team{}, err
	}
	u.DefaultOrgID = orgID
	u.DefaultTeamID = teamID
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return identity.User{}, identity.Team{}, err
	}
	t, _ := s.store.GetTeam(ctx, teamID)
	return u, t, nil
}

// createPersonalTeam provisions a personal Org wrapping a personal Team
// for the given user, adds owner memberships at both levels, and returns
// the new (orgID, teamID). The personal org preserves the single-user
// UX: one org, one team, the user owns both. Slugs derive from the email
// local-part with a numeric suffix on collision (org and team share the
// base; the team slug is suffixed "-team" to keep the namespaces clean).
func (s *Service) createPersonalTeam(ctx context.Context, u identity.User) (orgID, teamID string, err error) {
	now := s.now().UTC()
	org, err := s.createPersonalOrg(ctx, u, now)
	if err != nil {
		return "", "", err
	}
	base := identity.SlugifyTeamName(u.Email)
	if base == "" {
		base = "team"
	}
	for attempt := 0; attempt < 10; attempt++ {
		slug := base
		if attempt > 0 {
			slug = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		t := identity.Team{
			ID:        uuid.NewString(),
			OrgID:     org.ID,
			Name:      defaultPersonalTeamName(u),
			Slug:      slug,
			CreatedBy: u.ID,
			CreatedAt: now,
			UpdatedAt: now,
			Personal:  true,
		}
		_, err := s.store.CreateTeam(ctx, t)
		if errors.Is(err, identity.ErrSlugAlreadyTaken) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		if err := s.store.UpsertMembership(ctx, identity.Membership{
			UserID:   u.ID,
			TeamID:   t.ID,
			Role:     identity.RoleOwner,
			JoinedAt: now,
		}); err != nil {
			return "", "", err
		}
		return org.ID, t.ID, nil
	}
	return "", "", errors.New("auth: could not allocate slug for personal team")
}

// createPersonalOrg provisions an Org with Personal=true and an owner
// org-membership. Slug derives from the email local-part with a numeric
// suffix on collision.
func (s *Service) createPersonalOrg(ctx context.Context, u identity.User, now time.Time) (identity.Org, error) {
	base := identity.SlugifyTeamName(u.Email)
	if base == "" {
		base = "org"
	}
	for attempt := 0; attempt < 10; attempt++ {
		slug := base
		if attempt > 0 {
			slug = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		o := identity.Org{
			ID:        uuid.NewString(),
			Name:      defaultPersonalTeamName(u),
			Slug:      slug,
			CreatedBy: u.ID,
			CreatedAt: now,
			UpdatedAt: now,
			Personal:  true,
		}
		_, err := s.store.CreateOrg(ctx, o)
		if errors.Is(err, identity.ErrOrgSlugAlreadyTaken) {
			continue
		}
		if err != nil {
			return identity.Org{}, err
		}
		if err := s.store.UpsertOrgMembership(ctx, identity.OrgMembership{
			UserID:   u.ID,
			OrgID:    o.ID,
			Role:     identity.OrgRoleOwner,
			JoinedAt: now,
		}); err != nil {
			return identity.Org{}, err
		}
		return o, nil
	}
	return identity.Org{}, errors.New("auth: could not allocate slug for personal org")
}

func defaultPersonalTeamName(u identity.User) string {
	if u.Name != "" {
		return u.Name + "'s team"
	}
	return u.Email
}

// hashInvitationToken is a thin wrapper used only to keep this
// package self-contained without exposing the hashing scheme to
// callers.
func hashInvitationToken(token string) string {
	return HashRefreshToken(token)
}

// dummyHash is a pre-computed argon2id hash of a fixed string used
// to spend equivalent CPU on a not-found lookup so an attacker
// cannot enumerate accounts via timing. The plaintext is unknown to
// VerifyPassword, so it always returns (false, nil).
const dummyHash = "$argon2id$v=19$m=65536,t=2,p=1$YWFhYWFhYWFhYWFhYWFhYQ$" +
	"sBzKvHEqs+sN1iUqXQU8/lLrUgnExwa3JG6lFfCh9HE"

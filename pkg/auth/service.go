package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// Sentinel errors raised by Service. Handlers map them to HTTP
// statuses (Unauthorized, Forbidden, NotFound, Conflict).
var (
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrAccountDisabled    = errors.New("auth: account disabled")
	ErrPasswordWeak       = errors.New("auth: password too weak")
	ErrSignupClosed       = errors.New("auth: signup is invite-only")
	ErrInvitationNotFound = errors.New("auth: invitation not found")
	ErrInvitationMismatch = errors.New("auth: invitation does not match user")
	ErrTeamNotFound       = errors.New("auth: team not found")
	ErrNotAMember         = errors.New("auth: user is not a member of the team")
)

// SignupMode controls who may register without an invitation.
type SignupMode string

const (
	SignupOpen       SignupMode = "open"
	SignupInviteOnly SignupMode = "invite_only"
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
	now        func() time.Time
	refreshTTL time.Duration
}

// Config wires the Service.
type Config struct {
	Store      identity.Store
	Sessions   SessionStore
	Signer     *JWTSigner
	SignupMode SignupMode
	RefreshTTL time.Duration
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
	return &Service{
		store:      cfg.Store,
		sessions:   cfg.Sessions,
		signer:     cfg.Signer,
		signupMode: cfg.SignupMode,
		refreshTTL: cfg.RefreshTTL,
		now:        time.Now,
	}, nil
}

// LoginResult bundles the artifacts returned to the caller after a
// successful login or refresh. The HTTP layer translates these into
// cookies / JSON.
type LoginResult struct {
	User           identity.User
	ActiveTeamID   string
	ActiveRole     identity.Role
	AccessToken    string
	AccessExpires  time.Time
	RefreshToken   string
	RefreshExpires time.Time
	Memberships    []identity.Membership
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
		return LoginResult{}, ErrInvalidCredentials
	}
	ok, err := VerifyPassword(password, u.PasswordHash)
	if err != nil || !ok {
		return LoginResult{}, ErrInvalidCredentials
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

// issueLogin is the shared post-authentication path used by Login
// and AcceptInvitation. It picks the active team, mints tokens, and
// records the refresh session.
func (s *Service) issueLogin(ctx context.Context, u identity.User, userAgent, ip string) (LoginResult, error) {
	memberships, err := s.store.ListMembershipsByUser(ctx, u.ID)
	if err != nil {
		return LoginResult{}, err
	}
	teamID, role := s.pickActiveTeam(u, memberships)
	id := Identity{
		UserID:       u.ID,
		Email:        u.Email,
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
		ActiveTeamID:   teamID,
		ActiveRole:     role,
		AccessToken:    access,
		AccessExpires:  exp,
		RefreshToken:   refresh,
		RefreshExpires: sess.ExpiresAt,
		Memberships:    memberships,
	}, nil
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
		_ = s.sessions.RevokeUserSessions(ctx, prev.UserID, now)
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
		return LoginResult{}, ErrAccountDisabled
	}
	// Rotate.
	if err := s.sessions.RevokeSession(ctx, prev.ID, now); err != nil {
		return LoginResult{}, err
	}
	return s.issueLogin(ctx, u, userAgent, ip)
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

// SwitchTeam re-issues the access JWT bound to teamID. Validates
// that the current user is a member.
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
	id := Identity{
		UserID:       u.ID,
		Email:        u.Email,
		TeamID:       mb.TeamID,
		Role:         mb.Role,
		IsSuperAdmin: u.IsSuperAdmin,
	}
	tok, exp, err := s.signer.IssueAccess(id)
	if err != nil {
		return Identity{}, "", time.Time{}, err
	}
	// Persist the choice as the user's new default.
	u.DefaultTeamID = teamID
	_ = s.store.UpdateUser(ctx, u)
	return id, tok, exp, nil
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
	// Auto-create a personal team so the user has somewhere to land.
	teamID, err := s.createPersonalTeam(ctx, u)
	if err != nil {
		return LoginResult{}, err
	}
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
	u.DefaultTeamID = inv.TeamID
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

// CreateTeamFor provisions a non-personal team owned by user
// `userID`. Returns the new team. If slug is empty it is derived
// from name and uniquified with a numeric suffix on collision.
func (s *Service) CreateTeamFor(ctx context.Context, userID, name, slug string) (identity.Team, error) {
	if name == "" {
		return identity.Team{}, fmt.Errorf("auth: team name required")
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return identity.Team{}, err
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
	teamID, err := s.createPersonalTeam(ctx, u)
	if err != nil {
		return identity.User{}, identity.Team{}, err
	}
	u.DefaultTeamID = teamID
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return identity.User{}, identity.Team{}, err
	}
	t, _ := s.store.GetTeam(ctx, teamID)
	return u, t, nil
}

// createPersonalTeam provisions a Team with Personal=true for the
// given user, adds an Owner membership, and returns the team id.
// Slug is derived from the email local-part with a numeric suffix
// on collision.
func (s *Service) createPersonalTeam(ctx context.Context, u identity.User) (string, error) {
	base := identity.SlugifyTeamName(u.Email)
	if base == "" {
		base = "team"
	}
	now := s.now().UTC()
	for attempt := 0; attempt < 10; attempt++ {
		slug := base
		if attempt > 0 {
			slug = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		t := identity.Team{
			ID:        uuid.NewString(),
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
			return "", err
		}
		if err := s.store.UpsertMembership(ctx, identity.Membership{
			UserID:   u.ID,
			TeamID:   t.ID,
			Role:     identity.RoleOwner,
			JoinedAt: now,
		}); err != nil {
			return "", err
		}
		return t.ID, nil
	}
	return "", errors.New("auth: could not allocate slug for personal team")
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

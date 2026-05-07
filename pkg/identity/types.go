// Package identity owns the multitenant user/team/membership domain.
//
// Concepts:
//   - User: a person who can authenticate. Email is unique.
//   - Team: a tenant boundary. Every Run, ApiKey, Event, Audit entry
//     is partitioned by Team.ID.
//   - Membership: (User, Team, Role) triple. A user can belong to
//     many teams; the active team lives in the JWT.
//   - Invitation: pending offer to join a Team. Bearer-token URL
//     consumed by /auth/invitations/accept.
//   - OIDCLink: external IdP identity bound to a User (one per
//     provider). Used to log in without password.
//
// Storage is abstracted via Store; pkg/identity/mongo.go is the
// production impl, pkg/identity/memory.go is the in-process impl
// that other packages' tests use.
package identity

import (
	"errors"
	"strings"
	"time"
)

// Role is the per-team RBAC level. Order matters for comparison.
type Role string

const (
	RoleViewer Role = "viewer"
	RoleMember Role = "member"
	RoleAdmin  Role = "admin"
	RoleOwner  Role = "owner"
)

// rank gives a totally-ordered weight so callers can express
// "requires at least Admin" without a switch ladder.
func (r Role) rank() int {
	switch r {
	case RoleViewer:
		return 1
	case RoleMember:
		return 2
	case RoleAdmin:
		return 3
	case RoleOwner:
		return 4
	}
	return 0
}

// AtLeast reports whether r confers all permissions of want.
func (r Role) AtLeast(want Role) bool {
	if r.rank() == 0 {
		return false
	}
	return r.rank() >= want.rank()
}

// Valid reports whether r is one of the four known roles.
func (r Role) Valid() bool { return r.rank() > 0 }

// UserStatus tracks whether the user can log in. Disabled users
// retain their data but every login attempt is rejected.
type UserStatus string

const (
	UserStatusActive   UserStatus = "active"
	UserStatusDisabled UserStatus = "disabled"
	// UserStatusPendingPasswordChange forces the user to set a new
	// password on next login. Used by bootstrap admin and admin-
	// initiated resets.
	UserStatusPendingPasswordChange UserStatus = "pending_password_change"
)

// User is a person account.
type User struct {
	ID            string     `bson:"_id" json:"id"`
	Email         string     `bson:"email" json:"email"`
	Name          string     `bson:"name,omitempty" json:"name,omitempty"`
	PasswordHash  string     `bson:"password_hash,omitempty" json:"-"`
	Status        UserStatus `bson:"status" json:"status"`
	IsSuperAdmin  bool       `bson:"is_super_admin,omitempty" json:"is_super_admin,omitempty"`
	CreatedAt     time.Time  `bson:"created_at" json:"created_at"`
	UpdatedAt     time.Time  `bson:"updated_at" json:"updated_at"`
	LastLoginAt   *time.Time `bson:"last_login_at,omitempty" json:"last_login_at,omitempty"`
	FailedLogins  int        `bson:"failed_logins,omitempty" json:"-"`
	LockedUntil   *time.Time `bson:"locked_until,omitempty" json:"-"`
	DefaultTeamID string     `bson:"default_team_id,omitempty" json:"default_team_id,omitempty"`
}

// Team is a tenant. Every business object (run, key, event) is
// partitioned by Team.ID.
type Team struct {
	ID        string    `bson:"_id" json:"id"`
	Name      string    `bson:"name" json:"name"`
	Slug      string    `bson:"slug" json:"slug"`
	CreatedBy string    `bson:"created_by,omitempty" json:"created_by,omitempty"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
	// Personal is true for the team auto-created when a user signs
	// up without an invitation. Used to label the UI and to prevent
	// inviting other users into someone's personal space.
	Personal bool `bson:"personal,omitempty" json:"personal,omitempty"`
}

// Membership glues a user to a team with a role.
type Membership struct {
	UserID    string    `bson:"user_id" json:"user_id"`
	TeamID    string    `bson:"team_id" json:"team_id"`
	Role      Role      `bson:"role" json:"role"`
	InvitedBy string    `bson:"invited_by,omitempty" json:"invited_by,omitempty"`
	JoinedAt  time.Time `bson:"joined_at" json:"joined_at"`
}

// Invitation is a pending offer to join a team. The token surfaced
// in the email is hashed in TokenHash; we never store the plaintext.
type Invitation struct {
	ID         string     `bson:"_id" json:"id"`
	TeamID     string     `bson:"team_id" json:"team_id"`
	Email      string     `bson:"email" json:"email"`
	Role       Role       `bson:"role" json:"role"`
	TokenHash  string     `bson:"token_hash" json:"-"`
	InvitedBy  string     `bson:"invited_by" json:"invited_by"`
	CreatedAt  time.Time  `bson:"created_at" json:"created_at"`
	ExpiresAt  time.Time  `bson:"expires_at" json:"expires_at"`
	AcceptedAt *time.Time `bson:"accepted_at,omitempty" json:"accepted_at,omitempty"`
	AcceptedBy string     `bson:"accepted_by,omitempty" json:"accepted_by,omitempty"`
}

// OIDCLink binds a User to an external IdP identity. Composite key
// is (Provider, ProviderUserID); the linked user is the lookup
// target. A user can own multiple links (Google + GitHub).
type OIDCLink struct {
	Provider       string    `bson:"provider" json:"provider"`
	ProviderUserID string    `bson:"provider_user_id" json:"provider_user_id"`
	UserID         string    `bson:"user_id" json:"user_id"`
	Email          string    `bson:"email,omitempty" json:"email,omitempty"`
	CreatedAt      time.Time `bson:"created_at" json:"created_at"`
}

// Sentinel errors returned by every Store implementation. Handlers
// translate these to HTTP status codes (Not Found → 404, Conflict
// → 409, etc.) without leaking internals.
var (
	ErrNotFound          = errors.New("identity: not found")
	ErrEmailAlreadyTaken = errors.New("identity: email already taken")
	ErrSlugAlreadyTaken  = errors.New("identity: team slug already taken")
	ErrInvalidRole       = errors.New("identity: invalid role")
	ErrInvitationUsed    = errors.New("identity: invitation already accepted")
	ErrInvitationExpired = errors.New("identity: invitation expired")
)

// NormalizeEmail lower-cases and trims whitespace from an email.
// Used everywhere we read or write emails — the unique index on the
// users collection is on the normalized form.
func NormalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// SlugifyTeamName produces a URL-safe slug from a team name. We
// don't try to be exhaustive (no Unicode normalization) because the
// slug is also editable directly via the API; the API returns 409
// on collision and the operator picks another.
func SlugifyTeamName(name string) string {
	out := make([]rune, 0, len(name))
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z':
			out = append(out, r)
			prevDash = false
		case r >= '0' && r <= '9':
			out = append(out, r)
			prevDash = false
		case r == '-' || r == '_' || r == ' ':
			if !prevDash && len(out) > 0 {
				out = append(out, '-')
				prevDash = true
			}
		}
	}
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	return string(out)
}

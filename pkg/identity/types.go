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
	// RoleConfigEditor is an ORTHOGONAL least-privilege capability, NOT a rung
	// on the viewer<member<admin<owner ladder (ADR-078): it grants exactly one
	// thing — edit this team's config-shares — and nothing else. It ranks 0, so
	// AtLeast never places it in the ladder (it is neither ≥ viewer nor is any
	// ladder role ≥ it), yet Valid() accepts it so it round-trips through the
	// JWT claim and the member-management API. Standard team gates admit
	// AtLeast(RoleViewer) (equivalent to the old Valid() for the four ladder
	// roles), which excludes this; only canEditConfigShares admits it.
	RoleConfigEditor Role = "config_editor"
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
	haveRank := r.rank()
	wantRank := want.rank()
	if haveRank == 0 || wantRank == 0 {
		return false
	}
	return haveRank >= wantRank
}

// Valid reports whether r is an assignable role — the four ladder roles plus
// the orthogonal config_editor capability (rank 0, so ladder comparisons
// exclude it, but it is still a real, storable, JWT-round-trippable role).
func (r Role) Valid() bool { return r.rank() > 0 || r == RoleConfigEditor }

// OrgRole is the org-level RBAC level — coarser than the per-team
// Role: an org member is granted access to 0..N teams within the org,
// each with its own Role. Order matters for comparison.
type OrgRole string

const (
	OrgRoleMember OrgRole = "member"
	OrgRoleAdmin  OrgRole = "admin"
	OrgRoleOwner  OrgRole = "owner"
)

// rank gives a totally-ordered weight (mirrors Role.rank).
func (r OrgRole) rank() int {
	switch r {
	case OrgRoleMember:
		return 1
	case OrgRoleAdmin:
		return 2
	case OrgRoleOwner:
		return 3
	}
	return 0
}

// AtLeast reports whether r confers all permissions of want.
func (r OrgRole) AtLeast(want OrgRole) bool {
	haveRank := r.rank()
	wantRank := want.rank()
	if haveRank == 0 || wantRank == 0 {
		return false
	}
	return haveRank >= wantRank
}

// Valid reports whether r is one of the three known org roles.
func (r OrgRole) Valid() bool { return r.rank() > 0 }

// OrgRoleForTeamRole maps a team Role to the org role it implies when
// a team membership is mirrored up to the org (used on signup, invite,
// and the teams→orgs backfill): team owners/admins become org admins,
// everyone else an org member.
func OrgRoleForTeamRole(r Role) OrgRole {
	switch {
	case r.AtLeast(RoleAdmin):
		return OrgRoleAdmin
	case r.AtLeast(RoleViewer):
		return OrgRoleMember
	}
	// A capability OUTSIDE the ladder (config_editor, rank 0) confers no org
	// role — it must not mirror up to an org membership (ADR-078), else it
	// would pass canViewOrg. Login still resolves the org context with an empty
	// OrgRole, which the org gates reject.
	return ""
}

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
	// DefaultOrgID is the user's preferred active org for new sessions.
	DefaultOrgID string `bson:"default_org_id,omitempty" json:"default_org_id,omitempty"`
	// GitHubOrgs are the user's GitHub org logins, captured via a read:org grant
	// (the "create a GitHub App" org picker) or at GitHub SSO login. Used only as
	// UI hints (the org dropdown); never an authorization input.
	GitHubOrgs []string `bson:"github_orgs,omitempty" json:"github_orgs,omitempty"`
}

// TeamStatus controls whether a team (org) can run work. An empty
// value reads as active (back-compat for rows created before the field
// existed) — always go through Team.EffectiveStatus.
type TeamStatus string

const (
	TeamStatusActive    TeamStatus = "active"
	TeamStatusSuspended TeamStatus = "suspended" // no run launches; super-admin only
	TeamStatusReadOnly  TeamStatus = "read_only" // login + reads OK, no run launches
	// TeamStatusPendingDeletion marks an org soft-deleted: blocked like a
	// suspension, awaiting the nightly sweeper which hard-purges it once
	// Org.PurgeAfter has passed. Restorable until then. Org-only today.
	TeamStatusPendingDeletion TeamStatus = "pending_deletion"
)

// ValidTeamStatus reports whether s is one of the known statuses.
func ValidTeamStatus(s TeamStatus) bool {
	switch s {
	case TeamStatusActive, TeamStatusSuspended, TeamStatusReadOnly, TeamStatusPendingDeletion:
		return true
	}
	return false
}

// Team is a working sub-unit within an Org and the resource tenant:
// every business object (run, key, event, board, forge connection,
// secret) is partitioned by Team.ID. Members, SSO, billing and the
// monthly budget live one level up on the parent Org (see Org).
type Team struct {
	ID string `bson:"_id" json:"id"`
	// OrgID is the parent organization. Every team belongs to exactly
	// one Org; the teams→orgs backfill sets this on legacy rows.
	OrgID     string    `bson:"org_id,omitempty" json:"org_id,omitempty"`
	Name      string    `bson:"name" json:"name"`
	Slug      string    `bson:"slug" json:"slug"`
	CreatedBy string    `bson:"created_by,omitempty" json:"created_by,omitempty"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
	// Personal is true for the team auto-created when a user signs
	// up without an invitation. Used to label the UI and to prevent
	// inviting other users into someone's personal space.
	Personal bool `bson:"personal,omitempty" json:"personal,omitempty"`

	// Lifecycle (super-admin managed). Empty Status == active. A team
	// can be suspended independently of its org; the org's status gates
	// all of its teams.
	Status        TeamStatus `bson:"status,omitempty" json:"status,omitempty"`
	SuspendedAt   *time.Time `bson:"suspended_at,omitempty" json:"suspended_at,omitempty"`
	SuspendedBy   string     `bson:"suspended_by,omitempty" json:"suspended_by,omitempty"`
	SuspendReason string     `bson:"suspend_reason,omitempty" json:"suspend_reason,omitempty"`

	// Per-team executor caps. Zero means "inherit the platform default".
	// These stay team-level (they protect each workspace's executor);
	// the monthly run/cost budget is org-level (see Org).
	// MaxConcurrentRuns caps simultaneously active (queued + running)
	// runs for the team.
	MaxConcurrentRuns int `bson:"max_concurrent_runs,omitempty" json:"max_concurrent_runs,omitempty"`
	// LaunchRatePerMin caps run-launch requests per minute (token
	// bucket, burst = the same value).
	LaunchRatePerMin int `bson:"launch_rate_per_min,omitempty" json:"launch_rate_per_min,omitempty"`
}

// EffectiveStatus treats an empty status (legacy rows) as active.
func (t Team) EffectiveStatus() TeamStatus {
	if t.Status == "" {
		return TeamStatusActive
	}
	return t.Status
}

// Suspended reports whether the team is suspended (no run launches).
func (t Team) Suspended() bool { return t.EffectiveStatus() == TeamStatusSuspended }

// CanLaunch reports whether the team may launch new runs — false when
// suspended or read-only.
func (t Team) CanLaunch() bool {
	switch t.EffectiveStatus() {
	case TeamStatusSuspended, TeamStatusReadOnly:
		return false
	default:
		return true
	}
}

// Org is the top-level tenant grouping: the billing/identity boundary
// that owns SSO, the member roster, the plan + monthly budget, and
// control-plane audit. Resources stay partitioned by Team.ID; an Org
// groups one or more Teams (see Team.OrgID).
type Org struct {
	ID        string    `bson:"_id" json:"id"`
	Name      string    `bson:"name" json:"name"`
	Slug      string    `bson:"slug" json:"slug"`
	CreatedBy string    `bson:"created_by,omitempty" json:"created_by,omitempty"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
	// Personal is true for the org auto-created on a password signup
	// (one personal org wrapping one personal team) — preserves the
	// single-user UX. Personal orgs reject member invitations.
	Personal bool `bson:"personal,omitempty" json:"personal,omitempty"`
	// MigratedFromTeamID records the source team when this org was
	// created by the teams→orgs backfill. It is the idempotency key
	// (re-running the backfill finds the existing org) and the handle
	// the --reverse path uses to undo the migration.
	MigratedFromTeamID string `bson:"migrated_from_team_id,omitempty" json:"migrated_from_team_id,omitempty"`

	// Lifecycle (super-admin managed). Empty Status == active. An org's
	// status gates every team within it.
	Status        TeamStatus `bson:"status,omitempty" json:"status,omitempty"`
	SuspendedAt   *time.Time `bson:"suspended_at,omitempty" json:"suspended_at,omitempty"`
	SuspendedBy   string     `bson:"suspended_by,omitempty" json:"suspended_by,omitempty"`
	SuspendReason string     `bson:"suspend_reason,omitempty" json:"suspend_reason,omitempty"`

	// PurgeAfter is set when the org is soft-deleted (Status ==
	// pending_deletion): the instant the nightly sweeper may hard-purge it.
	// Cleared on restore. nil for orgs not pending deletion.
	PurgeAfter *time.Time `bson:"purge_after,omitempty" json:"purge_after,omitempty"`

	// Monthly budget (super-admin managed). Zero means "inherit the
	// platform default". Enforced pre-launch against the org-keyed
	// usage counter, which sums every team in the org.
	MonthlyRunQuota  int   `bson:"monthly_run_quota,omitempty" json:"monthly_run_quota,omitempty"`
	MemoryQuotaBytes int64 `bson:"memory_quota_bytes,omitempty" json:"memory_quota_bytes,omitempty"`
	// MonthlyCostCapUSD caps the month's metered LLM spend in USD
	// across the whole org (a soft cap: in-flight runs finish; new
	// launches are denied once crossed).
	MonthlyCostCapUSD float64 `bson:"monthly_cost_cap_usd,omitempty" json:"monthly_cost_cap_usd,omitempty"`

	// RequireProvisionApproval (org-admin managed) parks any repo-bot
	// provisioning requested by a TEAM admin as a pending approval an ORG
	// admin must approve before anything is created forge-side. Org
	// admins (and super-admins) provisioning themselves are not gated.
	// Off by default — existing orgs keep the direct-provision behaviour.
	RequireProvisionApproval bool `bson:"require_provision_approval,omitempty" json:"require_provision_approval,omitempty"`
}

// EffectiveStatus treats an empty status (legacy rows) as active.
func (o Org) EffectiveStatus() TeamStatus {
	if o.Status == "" {
		return TeamStatusActive
	}
	return o.Status
}

// Suspended reports whether the org is blocked from normal use — true for
// an explicit suspension AND for a pending-deletion org (soft-deleted, awaiting
// purge), so every gate that blocks on suspension also blocks the latter.
func (o Org) Suspended() bool {
	s := o.EffectiveStatus()
	return s == TeamStatusSuspended || s == TeamStatusPendingDeletion
}

// PendingDeletion reports whether the org is soft-deleted and awaiting purge.
func (o Org) PendingDeletion() bool { return o.EffectiveStatus() == TeamStatusPendingDeletion }

// CanLaunch reports whether the org may launch new runs — false when
// suspended, read-only, or pending deletion.
func (o Org) CanLaunch() bool {
	switch o.EffectiveStatus() {
	case TeamStatusSuspended, TeamStatusReadOnly, TeamStatusPendingDeletion:
		return false
	default:
		return true
	}
}

// OrgMembership glues a user to an org with an org-level role. It is
// the billing/SSO identity: you must be an org member to be granted a
// team within it (see Membership). A user can be an org member with
// zero team grants.
type OrgMembership struct {
	UserID   string    `bson:"user_id" json:"user_id"`
	OrgID    string    `bson:"org_id" json:"org_id"`
	Role     OrgRole   `bson:"role" json:"role"`
	Source   string    `bson:"source,omitempty" json:"source,omitempty"`
	JoinedAt time.Time `bson:"joined_at" json:"joined_at"`
}

// Membership glues a user to a team with a role.
type Membership struct {
	UserID    string    `bson:"user_id" json:"user_id"`
	TeamID    string    `bson:"team_id" json:"team_id"`
	Role      Role      `bson:"role" json:"role"`
	InvitedBy string    `bson:"invited_by,omitempty" json:"invited_by,omitempty"`
	JoinedAt  time.Time `bson:"joined_at" json:"joined_at"`
	// Source records how the membership was created, so SSO-minted memberships
	// can be re-evaluated/revoked on a later login without disturbing
	// human-created ones. Empty = legacy/manual/invitation — NEVER auto-revoked.
	Source string `bson:"source,omitempty" json:"source,omitempty"`
}

// Membership provenance values for Membership.Source.
const (
	MembershipSourceGitHubSSO = "github_sso"
	MembershipSourceOIDCSSO   = "oidc_sso"
)

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
	ErrNotFound            = errors.New("identity: not found")
	ErrEmailAlreadyTaken   = errors.New("identity: email already taken")
	ErrSlugAlreadyTaken    = errors.New("identity: team slug already taken")
	ErrOrgSlugAlreadyTaken = errors.New("identity: org slug already taken")
	ErrInvalidRole         = errors.New("identity: invalid role")
	ErrInvitationUsed      = errors.New("identity: invitation already accepted")
	ErrInvitationExpired   = errors.New("identity: invitation expired")
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

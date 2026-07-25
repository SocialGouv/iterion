package identity

import (
	"context"
	"time"
)

// Store is the persistence interface for the identity domain. The
// Mongo implementation lives in mongo.go; an in-memory variant in
// memory.go powers other packages' unit tests without a live DB.
//
// All methods accept a context. Operations that hit the network must
// respect the context's deadline.
type Store interface {
	// Users
	CreateUser(ctx context.Context, u User) (User, error)
	GetUser(ctx context.Context, id string) (User, error)
	GetUserByEmail(ctx context.Context, email string) (User, error)
	// GetUsersByIDs returns the users matching ids, keyed by id. An id
	// with no matching user is simply absent from the result — the bulk
	// analogue of a per-id GetUser ErrNotFound — so callers resolving a
	// reference list (member rosters, org trees) issue one query instead
	// of a Get per row.
	GetUsersByIDs(ctx context.Context, ids []string) (map[string]User, error)
	UpdateUser(ctx context.Context, u User) error
	ListUsers(ctx context.Context, page Page) ([]User, error)
	UserCount(ctx context.Context) (int64, error)

	// Orgs
	CreateOrg(ctx context.Context, o Org) (Org, error)
	GetOrg(ctx context.Context, id string) (Org, error)
	GetOrgBySlug(ctx context.Context, slug string) (Org, error)
	// GetOrgsByIDs returns the orgs matching ids, keyed by id; missing
	// ids are absent from the result (see GetUsersByIDs).
	GetOrgsByIDs(ctx context.Context, ids []string) (map[string]Org, error)
	UpdateOrg(ctx context.Context, o Org) error
	// DeleteOrg removes an org. Orgs are never deleted in normal
	// operation; this exists for the teams→orgs backfill's --reverse path.
	DeleteOrg(ctx context.Context, id string) error
	// ListOrgs returns all orgs (super-admin console), oldest first,
	// offset/limit paginated.
	ListOrgs(ctx context.Context, page Page) ([]Org, error)
	// ListOrgsPendingPurge returns orgs soft-deleted (Status ==
	// pending_deletion) whose PurgeAfter is at or before `before` — the
	// nightly purge sweeper's work list.
	ListOrgsPendingPurge(ctx context.Context, before time.Time) ([]Org, error)

	// Teams
	CreateTeam(ctx context.Context, t Team) (Team, error)
	GetTeam(ctx context.Context, id string) (Team, error)
	GetTeamBySlug(ctx context.Context, slug string) (Team, error)
	// GetTeamsByIDs returns the teams matching ids, keyed by id; missing
	// ids are absent from the result (see GetUsersByIDs).
	GetTeamsByIDs(ctx context.Context, ids []string) (map[string]Team, error)
	UpdateTeam(ctx context.Context, t Team) error
	// DeleteTeam removes a team. Like DeleteOrg this is not part of
	// normal operation; it backs super-admin org deletion (cascade) and
	// the teams→orgs backfill reversal. Team-scoped resources in other
	// stores (runs, board, forge connections) are not purged here.
	DeleteTeam(ctx context.Context, id string) error
	// ListTeams returns all teams (super-admin org console), oldest
	// first, offset/limit paginated.
	ListTeams(ctx context.Context, page Page) ([]Team, error)
	// ListTeamsByOrg returns every team belonging to one org.
	ListTeamsByOrg(ctx context.Context, orgID string) ([]Team, error)

	// Org memberships
	UpsertOrgMembership(ctx context.Context, m OrgMembership) error
	GetOrgMembership(ctx context.Context, userID, orgID string) (OrgMembership, error)
	DeleteOrgMembership(ctx context.Context, userID, orgID string) error
	ListOrgMembershipsByUser(ctx context.Context, userID string) ([]OrgMembership, error)
	ListOrgMembershipsByOrg(ctx context.Context, orgID string) ([]OrgMembership, error)

	// Memberships
	UpsertMembership(ctx context.Context, m Membership) error
	GetMembership(ctx context.Context, userID, teamID string) (Membership, error)
	DeleteMembership(ctx context.Context, userID, teamID string) error
	ListMembershipsByUser(ctx context.Context, userID string) ([]Membership, error)
	ListMembershipsByTeam(ctx context.Context, teamID string) ([]Membership, error)

	// Invitations
	CreateInvitation(ctx context.Context, inv Invitation) error
	GetInvitation(ctx context.Context, id string) (Invitation, error)
	GetInvitationByTokenHash(ctx context.Context, tokenHash string) (Invitation, error)
	UpdateInvitation(ctx context.Context, inv Invitation) error
	DeleteInvitation(ctx context.Context, id string) error
	ListInvitationsByTeam(ctx context.Context, teamID string) ([]Invitation, error)

	// OIDC links
	UpsertOIDCLink(ctx context.Context, link OIDCLink) error
	GetOIDCLink(ctx context.Context, provider, providerUserID string) (OIDCLink, error)
	ListOIDCLinksByUser(ctx context.Context, userID string) ([]OIDCLink, error)
	DeleteOIDCLink(ctx context.Context, provider, providerUserID string) error
}

// Page is a simple offset/limit cursor used by list endpoints.
// Limit==0 falls back to the per-method default.
type Page struct {
	Offset int
	Limit  int
}

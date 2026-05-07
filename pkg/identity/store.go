package identity

import "context"

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
	UpdateUser(ctx context.Context, u User) error
	ListUsers(ctx context.Context, page Page) ([]User, error)
	UserCount(ctx context.Context) (int64, error)

	// Teams
	CreateTeam(ctx context.Context, t Team) (Team, error)
	GetTeam(ctx context.Context, id string) (Team, error)
	GetTeamBySlug(ctx context.Context, slug string) (Team, error)
	UpdateTeam(ctx context.Context, t Team) error

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

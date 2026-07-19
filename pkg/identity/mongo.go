package identity

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// Collection names. Pinned constants so monitoring + migration
// tooling have a stable target.
const (
	colUsers          = "users"
	colOrgs           = "orgs"
	colOrgMemberships = "org_memberships"
	colTeams          = "teams"
	colMemberships    = "memberships"
	colInvitations    = "invitations"
	colOIDCLinks      = "oidc_links"
)

// MongoStore implements Store on top of MongoDB.
type MongoStore struct {
	db             *mongo.Database
	users          *mongo.Collection
	orgs           *mongo.Collection
	orgMemberships *mongo.Collection
	teams          *mongo.Collection
	memberships    *mongo.Collection
	invitations    *mongo.Collection
	oidcLinks      *mongo.Collection
}

// NewMongoStore returns a MongoStore wired to the given database.
// EnsureSchema must be called once before serving traffic.
func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{
		db:             db,
		users:          db.Collection(colUsers),
		orgs:           db.Collection(colOrgs),
		orgMemberships: db.Collection(colOrgMemberships),
		teams:          db.Collection(colTeams),
		memberships:    db.Collection(colMemberships),
		invitations:    db.Collection(colInvitations),
		oidcLinks:      db.Collection(colOIDCLinks),
	}
}

// EnsureSchema creates required indexes idempotently. Safe to run on
// every server boot.
func (s *MongoStore) EnsureSchema(ctx context.Context) error {
	if _, err := s.users.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetUnique(true).SetName("email_unique")},
		{Keys: bson.D{{Key: "status", Value: 1}}, Options: options.Index().SetName("status")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("identity: ensure users indexes: %w", err)
	}
	if _, err := s.orgs.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "slug", Value: 1}}, Options: options.Index().SetUnique(true).SetName("slug_unique")},
		{Keys: bson.D{{Key: "created_at", Value: -1}}, Options: options.Index().SetName("created_desc")},
		{Keys: bson.D{{Key: "status", Value: 1}}, Options: options.Index().SetName("status")},
		{Keys: bson.D{{Key: "migrated_from_team_id", Value: 1}}, Options: options.Index().SetUnique(true).SetName("migrated_from_team_unique").SetPartialFilterExpression(bson.M{"migrated_from_team_id": bson.M{"$exists": true}})},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("identity: ensure orgs indexes: %w", err)
	}
	if _, err := s.orgMemberships.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "org_id", Value: 1}}, Options: options.Index().SetUnique(true).SetName("user_org_unique")},
		{Keys: bson.D{{Key: "org_id", Value: 1}, {Key: "role", Value: 1}}, Options: options.Index().SetName("org_role")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("identity: ensure org_memberships indexes: %w", err)
	}
	if _, err := s.teams.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "slug", Value: 1}}, Options: options.Index().SetUnique(true).SetName("slug_unique")},
		{Keys: bson.D{{Key: "created_at", Value: -1}}, Options: options.Index().SetName("created_desc")},
		{Keys: bson.D{{Key: "status", Value: 1}}, Options: options.Index().SetName("status")},
		{Keys: bson.D{{Key: "org_id", Value: 1}}, Options: options.Index().SetName("org_id")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("identity: ensure teams indexes: %w", err)
	}
	if _, err := s.memberships.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "team_id", Value: 1}}, Options: options.Index().SetUnique(true).SetName("user_team_unique")},
		{Keys: bson.D{{Key: "team_id", Value: 1}, {Key: "role", Value: 1}}, Options: options.Index().SetName("team_role")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("identity: ensure memberships indexes: %w", err)
	}
	if _, err := s.invitations.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "token_hash", Value: 1}}, Options: options.Index().SetUnique(true).SetName("token_hash_unique").SetPartialFilterExpression(bson.M{"token_hash": bson.M{"$exists": true}})},
		{Keys: bson.D{{Key: "team_id", Value: 1}, {Key: "email", Value: 1}}, Options: options.Index().SetName("team_email")},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetName("invitations_ttl").SetExpireAfterSeconds(0)},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("identity: ensure invitations indexes: %w", err)
	}
	if _, err := s.oidcLinks.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "provider", Value: 1}, {Key: "provider_user_id", Value: 1}}, Options: options.Index().SetUnique(true).SetName("provider_subject_unique")},
		{Keys: bson.D{{Key: "user_id", Value: 1}}, Options: options.Index().SetName("user_id")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("identity: ensure oidc_links indexes: %w", err)
	}
	return nil
}

// findByIDs runs coll.Find({_id: {$in: ids}}) and returns the matching
// documents keyed by id (via idOf). Missing ids are simply absent from
// the map — the bulk analogue of a per-id ErrNotFound — and an empty
// ids slice short-circuits without a query. Shared by the MongoStore
// Get*ByIDs methods.
func findByIDs[T any](ctx context.Context, coll *mongo.Collection, ids []string, idOf func(T) string, findErrMsg, decodeErrMsg string) (map[string]T, error) {
	out := make(map[string]T, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	cur, err := coll.Find(ctx, bson.M{"_id": bson.M{"$in": ids}})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", findErrMsg, err)
	}
	defer cur.Close(ctx)
	var docs []T
	if err := cur.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("%s: %w", decodeErrMsg, err)
	}
	for _, d := range docs {
		out[idOf(d)] = d
	}
	return out, nil
}

// ----- Users -----

func (s *MongoStore) CreateUser(ctx context.Context, u User) (User, error) {
	u.Email = NormalizeEmail(u.Email)
	if _, err := s.users.InsertOne(ctx, u); err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return User{}, ErrEmailAlreadyTaken
		}
		return User{}, fmt.Errorf("identity: insert user: %w", err)
	}
	return u, nil
}

func (s *MongoStore) GetUser(ctx context.Context, id string) (User, error) {
	return mongoutil.FindOne[User](ctx, s.users, bson.M{"_id": id}, ErrNotFound, "identity: get user")
}

func (s *MongoStore) GetUserByEmail(ctx context.Context, email string) (User, error) {
	email = NormalizeEmail(email)
	return mongoutil.FindOne[User](ctx, s.users, bson.M{"email": email}, ErrNotFound, "identity: get user by email")
}

func (s *MongoStore) GetUsersByIDs(ctx context.Context, ids []string) (map[string]User, error) {
	return findByIDs(ctx, s.users, ids, func(u User) string { return u.ID },
		"identity: get users by ids", "identity: decode users by ids")
}

func (s *MongoStore) UpdateUser(ctx context.Context, u User) error {
	u.Email = NormalizeEmail(u.Email)
	return mongoutil.ReplaceOneChecked(ctx, s.users, bson.M{"_id": u.ID}, u, ErrEmailAlreadyTaken, ErrNotFound, "identity: update user")
}

func (s *MongoStore) ListUsers(ctx context.Context, page Page) ([]User, error) {
	skip, limit := mongoutil.NormalizePage(page.Offset, page.Limit, 50)
	return mongoutil.FindPageSorted[User](ctx, s.users, bson.M{}, "created_at", skip, limit,
		"identity: list users", "identity: decode users")
}

func (s *MongoStore) UserCount(ctx context.Context) (int64, error) {
	n, err := s.users.CountDocuments(ctx, bson.M{})
	if err != nil {
		return 0, fmt.Errorf("identity: count users: %w", err)
	}
	return n, nil
}

// ----- Teams -----

func (s *MongoStore) CreateTeam(ctx context.Context, t Team) (Team, error) {
	if _, err := s.teams.InsertOne(ctx, t); err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return Team{}, ErrSlugAlreadyTaken
		}
		return Team{}, fmt.Errorf("identity: insert team: %w", err)
	}
	return t, nil
}

func (s *MongoStore) GetTeam(ctx context.Context, id string) (Team, error) {
	return mongoutil.FindOne[Team](ctx, s.teams, bson.M{"_id": id}, ErrNotFound, "identity: get team")
}

func (s *MongoStore) GetTeamBySlug(ctx context.Context, slug string) (Team, error) {
	return mongoutil.FindOne[Team](ctx, s.teams, bson.M{"slug": slug}, ErrNotFound, "identity: get team by slug")
}

func (s *MongoStore) GetTeamsByIDs(ctx context.Context, ids []string) (map[string]Team, error) {
	return findByIDs(ctx, s.teams, ids, func(t Team) string { return t.ID },
		"identity: get teams by ids", "identity: decode teams by ids")
}

func (s *MongoStore) UpdateTeam(ctx context.Context, t Team) error {
	return mongoutil.ReplaceOneChecked(ctx, s.teams, bson.M{"_id": t.ID}, t, ErrSlugAlreadyTaken, ErrNotFound, "identity: update team")
}

func (s *MongoStore) DeleteTeam(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.teams, bson.M{"_id": id}, ErrNotFound, "identity: delete team")
}

func (s *MongoStore) ListTeams(ctx context.Context, page Page) ([]Team, error) {
	skip, limit := mongoutil.NormalizePage(page.Offset, page.Limit, 50)
	return mongoutil.FindPageSorted[Team](ctx, s.teams, bson.M{}, "created_at", skip, limit,
		"identity: list teams", "identity: decode teams")
}

func (s *MongoStore) ListTeamsByOrg(ctx context.Context, orgID string) ([]Team, error) {
	return mongoutil.FindAllSorted[Team](ctx, s.teams, bson.M{"org_id": orgID}, "created_at",
		"identity: list teams by org", "identity: decode teams")
}

// ----- Orgs -----

func (s *MongoStore) CreateOrg(ctx context.Context, o Org) (Org, error) {
	if _, err := s.orgs.InsertOne(ctx, o); err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return Org{}, ErrOrgSlugAlreadyTaken
		}
		return Org{}, fmt.Errorf("identity: insert org: %w", err)
	}
	return o, nil
}

func (s *MongoStore) GetOrg(ctx context.Context, id string) (Org, error) {
	return mongoutil.FindOne[Org](ctx, s.orgs, bson.M{"_id": id}, ErrNotFound, "identity: get org")
}

func (s *MongoStore) GetOrgBySlug(ctx context.Context, slug string) (Org, error) {
	return mongoutil.FindOne[Org](ctx, s.orgs, bson.M{"slug": slug}, ErrNotFound, "identity: get org by slug")
}

func (s *MongoStore) GetOrgsByIDs(ctx context.Context, ids []string) (map[string]Org, error) {
	return findByIDs(ctx, s.orgs, ids, func(o Org) string { return o.ID },
		"identity: get orgs by ids", "identity: decode orgs by ids")
}

func (s *MongoStore) UpdateOrg(ctx context.Context, o Org) error {
	return mongoutil.ReplaceOneChecked(ctx, s.orgs, bson.M{"_id": o.ID}, o, ErrOrgSlugAlreadyTaken, ErrNotFound, "identity: update org")
}

func (s *MongoStore) DeleteOrg(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.orgs, bson.M{"_id": id}, ErrNotFound, "identity: delete org")
}

func (s *MongoStore) ListOrgsPendingPurge(ctx context.Context, before time.Time) ([]Org, error) {
	cur, err := s.orgs.Find(ctx, bson.M{
		"status":      string(TeamStatusPendingDeletion),
		"purge_after": bson.M{"$lte": before},
	})
	if err != nil {
		return nil, fmt.Errorf("identity: list orgs pending purge: %w", err)
	}
	defer cur.Close(ctx)
	var out []Org
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("identity: decode orgs pending purge: %w", err)
	}
	return out, nil
}

func (s *MongoStore) ListOrgs(ctx context.Context, page Page) ([]Org, error) {
	skip, limit := mongoutil.NormalizePage(page.Offset, page.Limit, 50)
	return mongoutil.FindPageSorted[Org](ctx, s.orgs, bson.M{}, "created_at", skip, limit,
		"identity: list orgs", "identity: decode orgs")
}

// ----- Org memberships -----

func (s *MongoStore) UpsertOrgMembership(ctx context.Context, m OrgMembership) error {
	if !m.Role.Valid() {
		return ErrInvalidRole
	}
	filter := bson.M{"user_id": m.UserID, "org_id": m.OrgID}
	update := bson.M{"$set": m}
	_, err := s.orgMemberships.UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("identity: upsert org membership: %w", err)
	}
	return nil
}

func (s *MongoStore) GetOrgMembership(ctx context.Context, userID, orgID string) (OrgMembership, error) {
	return mongoutil.FindOne[OrgMembership](ctx, s.orgMemberships, bson.M{"user_id": userID, "org_id": orgID}, ErrNotFound, "identity: get org membership")
}

func (s *MongoStore) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	_, err := s.orgMemberships.DeleteOne(ctx, bson.M{"user_id": userID, "org_id": orgID})
	if err != nil {
		return fmt.Errorf("identity: delete org membership: %w", err)
	}
	return nil
}

func (s *MongoStore) ListOrgMembershipsByUser(ctx context.Context, userID string) ([]OrgMembership, error) {
	return mongoutil.FindAllSorted[OrgMembership](ctx, s.orgMemberships, bson.M{"user_id": userID}, "joined_at",
		"identity: list org memberships by user", "identity: decode org memberships")
}

func (s *MongoStore) ListOrgMembershipsByOrg(ctx context.Context, orgID string) ([]OrgMembership, error) {
	return mongoutil.FindAllSorted[OrgMembership](ctx, s.orgMemberships, bson.M{"org_id": orgID}, "joined_at",
		"identity: list org memberships by org", "identity: decode org memberships")
}

// ----- Memberships -----

func (s *MongoStore) UpsertMembership(ctx context.Context, m Membership) error {
	if !m.Role.Valid() {
		return ErrInvalidRole
	}
	filter := bson.M{"user_id": m.UserID, "team_id": m.TeamID}
	update := bson.M{"$set": m}
	_, err := s.memberships.UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("identity: upsert membership: %w", err)
	}
	return nil
}

func (s *MongoStore) GetMembership(ctx context.Context, userID, teamID string) (Membership, error) {
	return mongoutil.FindOne[Membership](ctx, s.memberships, bson.M{"user_id": userID, "team_id": teamID}, ErrNotFound, "identity: get membership")
}

func (s *MongoStore) DeleteMembership(ctx context.Context, userID, teamID string) error {
	_, err := s.memberships.DeleteOne(ctx, bson.M{"user_id": userID, "team_id": teamID})
	if err != nil {
		return fmt.Errorf("identity: delete membership: %w", err)
	}
	return nil
}

func (s *MongoStore) ListMembershipsByUser(ctx context.Context, userID string) ([]Membership, error) {
	return mongoutil.FindAllSorted[Membership](ctx, s.memberships, bson.M{"user_id": userID}, "joined_at",
		"identity: list memberships by user", "identity: decode memberships")
}

func (s *MongoStore) ListMembershipsByTeam(ctx context.Context, teamID string) ([]Membership, error) {
	return mongoutil.FindAllSorted[Membership](ctx, s.memberships, bson.M{"team_id": teamID}, "joined_at",
		"identity: list memberships by team", "identity: decode memberships")
}

// ----- Invitations -----

func (s *MongoStore) CreateInvitation(ctx context.Context, inv Invitation) error {
	_, err := s.invitations.InsertOne(ctx, inv)
	if err != nil {
		return fmt.Errorf("identity: insert invitation: %w", err)
	}
	return nil
}

func (s *MongoStore) GetInvitation(ctx context.Context, id string) (Invitation, error) {
	return mongoutil.FindOne[Invitation](ctx, s.invitations, bson.M{"_id": id}, ErrNotFound, "identity: get invitation")
}

func (s *MongoStore) GetInvitationByTokenHash(ctx context.Context, tokenHash string) (Invitation, error) {
	return mongoutil.FindOne[Invitation](ctx, s.invitations, bson.M{"token_hash": tokenHash}, ErrNotFound, "identity: get invitation by token")
}

func (s *MongoStore) UpdateInvitation(ctx context.Context, inv Invitation) error {
	return mongoutil.ReplaceOneChecked(ctx, s.invitations, bson.M{"_id": inv.ID}, inv, nil, ErrNotFound, "identity: update invitation")
}

func (s *MongoStore) DeleteInvitation(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.invitations, bson.M{"_id": id}, ErrNotFound, "identity: delete invitation")
}

func (s *MongoStore) ListInvitationsByTeam(ctx context.Context, teamID string) ([]Invitation, error) {
	return mongoutil.FindAllSorted[Invitation](ctx, s.invitations, bson.M{"team_id": teamID}, "created_at",
		"identity: list invitations", "identity: decode invitations")
}

// ----- OIDC links -----

func (s *MongoStore) UpsertOIDCLink(ctx context.Context, link OIDCLink) error {
	filter := bson.M{"provider": link.Provider, "provider_user_id": link.ProviderUserID}
	if link.CreatedAt.IsZero() {
		link.CreatedAt = time.Now().UTC()
	}
	update := bson.M{"$set": link}
	_, err := s.oidcLinks.UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("identity: upsert oidc link: %w", err)
	}
	return nil
}

func (s *MongoStore) GetOIDCLink(ctx context.Context, provider, providerUserID string) (OIDCLink, error) {
	return mongoutil.FindOne[OIDCLink](ctx, s.oidcLinks, bson.M{"provider": provider, "provider_user_id": providerUserID}, ErrNotFound, "identity: get oidc link")
}

func (s *MongoStore) ListOIDCLinksByUser(ctx context.Context, userID string) ([]OIDCLink, error) {
	return mongoutil.FindAllSorted[OIDCLink](ctx, s.oidcLinks, bson.M{"user_id": userID}, "provider",
		"identity: list oidc links", "identity: decode oidc links")
}

func (s *MongoStore) DeleteOIDCLink(ctx context.Context, provider, providerUserID string) error {
	return mongoutil.DeleteOneChecked(ctx, s.oidcLinks, bson.M{"provider": provider, "provider_user_id": providerUserID}, ErrNotFound, "identity: delete oidc link")
}

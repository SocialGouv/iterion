package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Collection names. Pinned constants so monitoring + migration
// tooling have a stable target.
const (
	colUsers       = "users"
	colTeams       = "teams"
	colMemberships = "memberships"
	colInvitations = "invitations"
	colOIDCLinks   = "oidc_links"
)

// MongoStore implements Store on top of MongoDB.
type MongoStore struct {
	db          *mongo.Database
	users       *mongo.Collection
	teams       *mongo.Collection
	memberships *mongo.Collection
	invitations *mongo.Collection
	oidcLinks   *mongo.Collection
}

// NewMongoStore returns a MongoStore wired to the given database.
// EnsureSchema must be called once before serving traffic.
func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{
		db:          db,
		users:       db.Collection(colUsers),
		teams:       db.Collection(colTeams),
		memberships: db.Collection(colMemberships),
		invitations: db.Collection(colInvitations),
		oidcLinks:   db.Collection(colOIDCLinks),
	}
}

// EnsureSchema creates required indexes idempotently. Safe to run on
// every server boot.
func (s *MongoStore) EnsureSchema(ctx context.Context) error {
	if _, err := s.users.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetUnique(true).SetName("email_unique")},
		{Keys: bson.D{{Key: "status", Value: 1}}, Options: options.Index().SetName("status")},
	}); err != nil && !isIndexConflict(err) {
		return fmt.Errorf("identity: ensure users indexes: %w", err)
	}
	if _, err := s.teams.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "slug", Value: 1}}, Options: options.Index().SetUnique(true).SetName("slug_unique")},
		{Keys: bson.D{{Key: "created_at", Value: -1}}, Options: options.Index().SetName("created_desc")},
	}); err != nil && !isIndexConflict(err) {
		return fmt.Errorf("identity: ensure teams indexes: %w", err)
	}
	if _, err := s.memberships.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "team_id", Value: 1}}, Options: options.Index().SetUnique(true).SetName("user_team_unique")},
		{Keys: bson.D{{Key: "team_id", Value: 1}, {Key: "role", Value: 1}}, Options: options.Index().SetName("team_role")},
	}); err != nil && !isIndexConflict(err) {
		return fmt.Errorf("identity: ensure memberships indexes: %w", err)
	}
	if _, err := s.invitations.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "token_hash", Value: 1}}, Options: options.Index().SetUnique(true).SetName("token_hash_unique").SetPartialFilterExpression(bson.M{"token_hash": bson.M{"$exists": true}})},
		{Keys: bson.D{{Key: "team_id", Value: 1}, {Key: "email", Value: 1}}, Options: options.Index().SetName("team_email")},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetName("invitations_ttl").SetExpireAfterSeconds(0)},
	}); err != nil && !isIndexConflict(err) {
		return fmt.Errorf("identity: ensure invitations indexes: %w", err)
	}
	if _, err := s.oidcLinks.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "provider", Value: 1}, {Key: "provider_user_id", Value: 1}}, Options: options.Index().SetUnique(true).SetName("provider_subject_unique")},
		{Keys: bson.D{{Key: "user_id", Value: 1}}, Options: options.Index().SetName("user_id")},
	}); err != nil && !isIndexConflict(err) {
		return fmt.Errorf("identity: ensure oidc_links indexes: %w", err)
	}
	return nil
}

// isIndexConflict treats benign re-creation collisions as no-ops so
// EnsureSchema is idempotent across binary upgrades.
func isIndexConflict(err error) bool {
	if err == nil {
		return false
	}
	var cmd mongo.CommandError
	if errors.As(err, &cmd) {
		switch cmd.Code {
		case 85, 86: // IndexOptionsConflict / IndexKeySpecsConflict
			return true
		}
	}
	return false
}

func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if mongo.IsDuplicateKeyError(err) {
		return true
	}
	return false
}

// ----- Users -----

func (s *MongoStore) CreateUser(ctx context.Context, u User) (User, error) {
	u.Email = NormalizeEmail(u.Email)
	if _, err := s.users.InsertOne(ctx, u); err != nil {
		if isDuplicateKey(err) {
			return User{}, ErrEmailAlreadyTaken
		}
		return User{}, fmt.Errorf("identity: insert user: %w", err)
	}
	return u, nil
}

func (s *MongoStore) GetUser(ctx context.Context, id string) (User, error) {
	var u User
	err := s.users.FindOne(ctx, bson.M{"_id": id}).Decode(&u)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("identity: get user: %w", err)
	}
	return u, nil
}

func (s *MongoStore) GetUserByEmail(ctx context.Context, email string) (User, error) {
	email = NormalizeEmail(email)
	var u User
	err := s.users.FindOne(ctx, bson.M{"email": email}).Decode(&u)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("identity: get user by email: %w", err)
	}
	return u, nil
}

func (s *MongoStore) UpdateUser(ctx context.Context, u User) error {
	u.Email = NormalizeEmail(u.Email)
	res, err := s.users.ReplaceOne(ctx, bson.M{"_id": u.ID}, u)
	if err != nil {
		if isDuplicateKey(err) {
			return ErrEmailAlreadyTaken
		}
		return fmt.Errorf("identity: update user: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoStore) ListUsers(ctx context.Context, page Page) ([]User, error) {
	limit := int64(page.Limit)
	if limit <= 0 {
		limit = 50
	}
	cur, err := s.users.Find(ctx, bson.M{}, options.Find().
		SetSort(bson.M{"created_at": 1}).
		SetSkip(int64(page.Offset)).
		SetLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("identity: list users: %w", err)
	}
	defer cur.Close(ctx)
	var out []User
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("identity: decode users: %w", err)
	}
	return out, nil
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
		if isDuplicateKey(err) {
			return Team{}, ErrSlugAlreadyTaken
		}
		return Team{}, fmt.Errorf("identity: insert team: %w", err)
	}
	return t, nil
}

func (s *MongoStore) GetTeam(ctx context.Context, id string) (Team, error) {
	var t Team
	err := s.teams.FindOne(ctx, bson.M{"_id": id}).Decode(&t)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Team{}, ErrNotFound
	}
	if err != nil {
		return Team{}, fmt.Errorf("identity: get team: %w", err)
	}
	return t, nil
}

func (s *MongoStore) GetTeamBySlug(ctx context.Context, slug string) (Team, error) {
	var t Team
	err := s.teams.FindOne(ctx, bson.M{"slug": slug}).Decode(&t)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Team{}, ErrNotFound
	}
	if err != nil {
		return Team{}, fmt.Errorf("identity: get team by slug: %w", err)
	}
	return t, nil
}

func (s *MongoStore) UpdateTeam(ctx context.Context, t Team) error {
	res, err := s.teams.ReplaceOne(ctx, bson.M{"_id": t.ID}, t)
	if err != nil {
		if isDuplicateKey(err) {
			return ErrSlugAlreadyTaken
		}
		return fmt.Errorf("identity: update team: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
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
	var m Membership
	err := s.memberships.FindOne(ctx, bson.M{"user_id": userID, "team_id": teamID}).Decode(&m)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Membership{}, ErrNotFound
	}
	if err != nil {
		return Membership{}, fmt.Errorf("identity: get membership: %w", err)
	}
	return m, nil
}

func (s *MongoStore) DeleteMembership(ctx context.Context, userID, teamID string) error {
	_, err := s.memberships.DeleteOne(ctx, bson.M{"user_id": userID, "team_id": teamID})
	if err != nil {
		return fmt.Errorf("identity: delete membership: %w", err)
	}
	return nil
}

func (s *MongoStore) ListMembershipsByUser(ctx context.Context, userID string) ([]Membership, error) {
	cur, err := s.memberships.Find(ctx, bson.M{"user_id": userID}, options.Find().SetSort(bson.M{"joined_at": 1}))
	if err != nil {
		return nil, fmt.Errorf("identity: list memberships by user: %w", err)
	}
	defer cur.Close(ctx)
	var out []Membership
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("identity: decode memberships: %w", err)
	}
	return out, nil
}

func (s *MongoStore) ListMembershipsByTeam(ctx context.Context, teamID string) ([]Membership, error) {
	cur, err := s.memberships.Find(ctx, bson.M{"team_id": teamID}, options.Find().SetSort(bson.M{"joined_at": 1}))
	if err != nil {
		return nil, fmt.Errorf("identity: list memberships by team: %w", err)
	}
	defer cur.Close(ctx)
	var out []Membership
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("identity: decode memberships: %w", err)
	}
	return out, nil
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
	var inv Invitation
	err := s.invitations.FindOne(ctx, bson.M{"_id": id}).Decode(&inv)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Invitation{}, ErrNotFound
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("identity: get invitation: %w", err)
	}
	return inv, nil
}

func (s *MongoStore) GetInvitationByTokenHash(ctx context.Context, tokenHash string) (Invitation, error) {
	var inv Invitation
	err := s.invitations.FindOne(ctx, bson.M{"token_hash": tokenHash}).Decode(&inv)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Invitation{}, ErrNotFound
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("identity: get invitation by token: %w", err)
	}
	return inv, nil
}

func (s *MongoStore) UpdateInvitation(ctx context.Context, inv Invitation) error {
	res, err := s.invitations.ReplaceOne(ctx, bson.M{"_id": inv.ID}, inv)
	if err != nil {
		return fmt.Errorf("identity: update invitation: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoStore) DeleteInvitation(ctx context.Context, id string) error {
	res, err := s.invitations.DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return fmt.Errorf("identity: delete invitation: %w", err)
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoStore) ListInvitationsByTeam(ctx context.Context, teamID string) ([]Invitation, error) {
	cur, err := s.invitations.Find(ctx, bson.M{"team_id": teamID}, options.Find().SetSort(bson.M{"created_at": 1}))
	if err != nil {
		return nil, fmt.Errorf("identity: list invitations: %w", err)
	}
	defer cur.Close(ctx)
	var out []Invitation
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("identity: decode invitations: %w", err)
	}
	return out, nil
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
	var link OIDCLink
	err := s.oidcLinks.FindOne(ctx, bson.M{"provider": provider, "provider_user_id": providerUserID}).Decode(&link)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return OIDCLink{}, ErrNotFound
	}
	if err != nil {
		return OIDCLink{}, fmt.Errorf("identity: get oidc link: %w", err)
	}
	return link, nil
}

func (s *MongoStore) ListOIDCLinksByUser(ctx context.Context, userID string) ([]OIDCLink, error) {
	cur, err := s.oidcLinks.Find(ctx, bson.M{"user_id": userID}, options.Find().SetSort(bson.M{"provider": 1}))
	if err != nil {
		return nil, fmt.Errorf("identity: list oidc links: %w", err)
	}
	defer cur.Close(ctx)
	var out []OIDCLink
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("identity: decode oidc links: %w", err)
	}
	return out, nil
}

func (s *MongoStore) DeleteOIDCLink(ctx context.Context, provider, providerUserID string) error {
	res, err := s.oidcLinks.DeleteOne(ctx, bson.M{"provider": provider, "provider_user_id": providerUserID})
	if err != nil {
		return fmt.Errorf("identity: delete oidc link: %w", err)
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

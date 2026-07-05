package orgsso

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// SSOProvidersCollectionName is the Mongo collection backing the per-tenant
// SSO provider rows.
const SSOProvidersCollectionName = "org_sso_providers"

// MongoStore is the production Store.
type MongoStore struct {
	coll *mongo.Collection
}

// NewMongoStore wires a MongoStore on the given database.
func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{coll: db.Collection(SSOProvidersCollectionName)}
}

// EnsureSchema creates the indexes:
//   - (tenant_id, kind): the general per-tenant listing index.
//   - partial-unique (tenant_id) where kind="github": at most one GitHub
//     gating row per org (a single allow-list; the Grants array composes
//     multiple GitHub orgs/teams inside it). A distinct key pattern from the
//     listing index so the two don't collide.
//   - partial multikey (github_team_keys) where kind="github" & enabled: the
//     reverse-lookup index that turns a GitHub login into one $in query.
func (s *MongoStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "tenant_id", Value: 1}, {Key: "kind", Value: 1}},
			Options: options.Index().SetName("tenant_kind"),
		},
		{
			Keys: bson.D{{Key: "tenant_id", Value: 1}},
			Options: options.Index().
				SetName("tenant_one_github_unique").
				SetUnique(true).
				SetPartialFilterExpression(bson.D{{Key: "kind", Value: string(KindGitHub)}}),
		},
		{
			Keys: bson.D{{Key: "github_team_keys", Value: 1}},
			Options: options.Index().
				SetName("github_team_keys_lookup").
				SetPartialFilterExpression(bson.D{
					{Key: "kind", Value: string(KindGitHub)},
					{Key: "enabled", Value: true},
				}),
		},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("orgsso: ensure org_sso_providers indexes: %w", err)
	}
	return nil
}

func (s *MongoStore) Create(ctx context.Context, p OrgSSOProvider) error {
	p.Normalize()
	if _, err := s.coll.InsertOne(ctx, p); err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return ErrExists
		}
		return fmt.Errorf("orgsso: insert provider: %w", err)
	}
	return nil
}

func (s *MongoStore) Get(ctx context.Context, id string) (OrgSSOProvider, error) {
	return mongoutil.FindOne[OrgSSOProvider](ctx, s.coll, bson.M{"_id": id}, ErrNotFound, "orgsso: get provider")
}

func (s *MongoStore) Update(ctx context.Context, p OrgSSOProvider) error {
	p.Normalize()
	return mongoutil.ReplaceOneChecked(ctx, s.coll, bson.M{"_id": p.ID}, p, ErrExists, ErrNotFound, "orgsso: update provider")
}

func (s *MongoStore) Delete(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll, bson.M{"_id": id}, ErrNotFound, "orgsso: delete provider")
}

func (s *MongoStore) ListByTenant(ctx context.Context, tenantID string) ([]OrgSSOProvider, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID})
}

func (s *MongoStore) ListByTenantKind(ctx context.Context, tenantID string, kind Kind) ([]OrgSSOProvider, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID, "kind": string(kind)})
}

func (s *MongoStore) GitHubGatingActive(ctx context.Context) (bool, error) {
	n, err := s.coll.CountDocuments(ctx, bson.M{"kind": string(KindGitHub), "enabled": true}, options.Count().SetLimit(1))
	if err != nil {
		return false, fmt.Errorf("orgsso: count github gates: %w", err)
	}
	return n > 0, nil
}

func (s *MongoStore) FindGitHubGrantingOrgs(ctx context.Context, keys []string) ([]OrgSSOProvider, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	return s.find(ctx, bson.M{
		"kind":             string(KindGitHub),
		"enabled":          true,
		"github_team_keys": bson.M{"$in": keys},
	})
}

func (s *MongoStore) find(ctx context.Context, filter bson.M) ([]OrgSSOProvider, error) {
	return mongoutil.FindAllSorted[OrgSSOProvider](ctx, s.coll, filter, "created_at",
		"orgsso: find providers", "orgsso: decode providers")
}

package trigger

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// SubscriptionsCollectionName is the Mongo collection backing the cloud-mode
// SubscriptionStore.
const SubscriptionsCollectionName = "trigger_subscriptions"

// MongoSubscriptionStore is the cloud-mode SubscriptionStore. Its index shape
// mirrors forge.MongoRepoIntegrationStore: a {tenant_id, repo} index for the
// candidate/by-repo queries and a {tenant_id, bot_id} index for by-bot.
type MongoSubscriptionStore struct {
	coll *mongo.Collection
}

func NewMongoSubscriptionStore(db *mongo.Database) *MongoSubscriptionStore {
	return &MongoSubscriptionStore{coll: db.Collection(SubscriptionsCollectionName)}
}

func (s *MongoSubscriptionStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "repo", Value: 1}}, Options: options.Index().SetName("tenant_repo")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "bot_id", Value: 1}}, Options: options.Index().SetName("tenant_bot")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "origin", Value: 1}}, Options: options.Index().SetName("tenant_origin")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("trigger: ensure subscriptions indexes: %w", err)
	}
	return nil
}

func (s *MongoSubscriptionStore) Create(ctx context.Context, sub Subscription) error {
	if _, err := s.coll.InsertOne(ctx, sub); err != nil {
		return fmt.Errorf("trigger: insert subscription: %w", err)
	}
	return nil
}

func (s *MongoSubscriptionStore) Get(ctx context.Context, id string) (Subscription, error) {
	return mongoutil.FindOne[Subscription](ctx, s.coll, bson.M{"_id": id}, ErrSubscriptionNotFound, "trigger: get subscription")
}

func (s *MongoSubscriptionStore) Update(ctx context.Context, sub Subscription) error {
	return mongoutil.ReplaceOneChecked(ctx, s.coll, bson.M{"_id": sub.ID}, sub, nil, ErrSubscriptionNotFound, "trigger: update subscription")
}

func (s *MongoSubscriptionStore) Delete(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll, bson.M{"_id": id}, ErrSubscriptionNotFound, "trigger: delete subscription")
}

func (s *MongoSubscriptionStore) ListByTenant(ctx context.Context, tenantID string) ([]Subscription, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID})
}

func (s *MongoSubscriptionStore) ListByRepo(ctx context.Context, tenantID, repo string) ([]Subscription, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID, "repo": bson.M{"$in": bson.A{repo, ""}}})
}

func (s *MongoSubscriptionStore) ListByBot(ctx context.Context, tenantID, botID string) ([]Subscription, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID, "bot_id": botID})
}

func (s *MongoSubscriptionStore) ListByOrigin(ctx context.Context, tenantID, origin string) ([]Subscription, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID, "origin": origin})
}

func (s *MongoSubscriptionStore) ListCandidates(ctx context.Context, ev Event) ([]Subscription, error) {
	return s.find(ctx, bson.M{
		"tenant_id": ev.TenantID,
		"enabled":   true,
		"repo":      bson.M{"$in": bson.A{ev.Repo, ""}},
	})
}

// DistinctBoardTenants returns the tenants holding at least one ENABLED
// board-kind subscription — the set the cloud board source poll-tails.
// Tenants without board triggers cost nothing.
func (s *MongoSubscriptionStore) DistinctBoardTenants(ctx context.Context) ([]string, error) {
	res := s.coll.Distinct(ctx, "tenant_id", bson.M{"invocation": "board", "enabled": true})
	var tenants []string
	if err := res.Decode(&tenants); err != nil {
		return nil, fmt.Errorf("trigger: distinct board tenants: %w", err)
	}
	return tenants, nil
}

func (s *MongoSubscriptionStore) find(ctx context.Context, filter bson.M) ([]Subscription, error) {
	return mongoutil.FindAllSorted[Subscription](ctx, s.coll, filter, "created_at",
		"trigger: list subscriptions", "trigger: decode subscriptions")
}

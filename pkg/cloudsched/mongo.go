package cloudsched

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// Collection is the Mongo collection name for scheduled bots.
const Collection = "scheduled_bots"

// MongoStore is the Mongo-backed Store.
type MongoStore struct {
	coll *mongo.Collection
}

// NewMongoStore builds a Mongo-backed scheduled-bot store.
func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{coll: db.Collection(Collection)}
}

var _ Store = (*MongoStore)(nil)

// EnsureSchema creates the indexes. Idempotent.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	coll := db.Collection(Collection)
	_, err := coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "repo_integration_id", Value: 1}}, Options: options.Index().SetName("tenant_integration")},
		{Keys: bson.D{{Key: "disabled", Value: 1}, {Key: "next_fire_at", Value: 1}}, Options: options.Index().SetName("due")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("cloudsched: ensure schema: %w", err)
	}
	return nil
}

func (s *MongoStore) Create(ctx context.Context, sb ScheduledBot) error {
	if _, err := s.coll.InsertOne(ctx, sb); err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return fmt.Errorf("cloudsched: schedule %q already exists", sb.ID)
		}
		return fmt.Errorf("cloudsched: create: %w", err)
	}
	return nil
}

func (s *MongoStore) Get(ctx context.Context, id string) (ScheduledBot, error) {
	return mongoutil.FindOne[ScheduledBot](ctx, s.coll, bson.M{"_id": id}, ErrNotFound, "cloudsched: get")
}

func (s *MongoStore) ListByIntegration(ctx context.Context, tenantID, integrationID string) ([]ScheduledBot, error) {
	cur, err := s.coll.Find(ctx, bson.M{"tenant_id": tenantID, "repo_integration_id": integrationID})
	if err != nil {
		return nil, fmt.Errorf("cloudsched: list by integration: %w", err)
	}
	var out []ScheduledBot
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("cloudsched: decode: %w", err)
	}
	return out, nil
}

func (s *MongoStore) ListDue(ctx context.Context, now time.Time, limit int) ([]ScheduledBot, error) {
	opt := options.Find().SetSort(bson.D{{Key: "next_fire_at", Value: 1}})
	if limit > 0 {
		opt.SetLimit(int64(limit))
	}
	cur, err := s.coll.Find(ctx, bson.M{
		"disabled":     bson.M{"$ne": true},
		"next_fire_at": bson.M{"$lte": now},
	}, opt)
	if err != nil {
		return nil, fmt.Errorf("cloudsched: list due: %w", err)
	}
	var out []ScheduledBot
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("cloudsched: decode: %w", err)
	}
	return out, nil
}

// ClaimTick is the CAS: the update matches only while next_fire_at still equals
// expectedNext, so the first replica to advance it wins and the rest get
// (false, nil). exactly-once per slot, no leader.
func (s *MongoStore) ClaimTick(ctx context.Context, id string, expectedNext, newNext, firedAt time.Time) (bool, error) {
	res, err := s.coll.UpdateOne(ctx,
		bson.M{"_id": id, "next_fire_at": expectedNext},
		bson.M{"$set": bson.M{"next_fire_at": newNext, "last_fire_at": firedAt, "updated_at": firedAt}},
	)
	if err != nil {
		return false, fmt.Errorf("cloudsched: claim tick: %w", err)
	}
	return res.MatchedCount > 0, nil
}

func (s *MongoStore) Delete(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll, bson.M{"_id": id}, ErrNotFound, "cloudsched: delete")
}

func (s *MongoStore) DeleteByIntegration(ctx context.Context, tenantID, integrationID string) error {
	_, err := s.coll.DeleteMany(ctx, bson.M{"tenant_id": tenantID, "repo_integration_id": integrationID})
	if err != nil {
		return fmt.Errorf("cloudsched: delete by integration: %w", err)
	}
	return nil
}

package audit

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/internal/storekit"
)

const colAudit = "audit_events"

// MongoStore is the production audit log.
type MongoStore struct{ kit *storekit.Mongo[Event] }

func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{kit: storekit.NewMongo[Event](db.Collection(colAudit), ErrNotFound, "audit")}
}

// EnsureSchema creates the audit indexes idempotently.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	col := db.Collection(colAudit)
	if _, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("tenant_recent")},
		{Keys: bson.D{{Key: "scope", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("scope_recent")},
		{Keys: bson.D{{Key: "action", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("action_recent")},
		{Keys: bson.D{{Key: "created_at", Value: 1}}, Options: options.Index().SetName("audit_ttl").SetExpireAfterSeconds(int32(RetentionDays * 24 * 60 * 60))},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("audit: ensure indexes: %w", err)
	}
	return nil
}

func (s *MongoStore) Insert(ctx context.Context, e Event) error {
	return s.kit.Insert(ctx, e, nil, "insert")
}

func (s *MongoStore) ListByTenant(ctx context.Context, tenantID string, p Page) ([]Event, error) {
	filter := bson.M{"tenant_id": tenantID, "scope": ScopeTenant}
	return s.list(ctx, filter, p)
}

func (s *MongoStore) ListPlatform(ctx context.Context, p Page) ([]Event, error) {
	return s.list(ctx, bson.M{"scope": ScopePlatform}, p)
}

func (s *MongoStore) list(ctx context.Context, filter bson.M, p Page) ([]Event, error) {
	if p.Action != "" {
		filter["action"] = p.Action
	}
	if p.ActorID != "" {
		filter["actor_id"] = p.ActorID
	}
	created := bson.M{}
	if !p.From.IsZero() {
		created["$gte"] = p.From
	}
	if !p.To.IsZero() {
		created["$lte"] = p.To
	}
	if len(created) > 0 {
		filter["created_at"] = created
	}
	return s.kit.List(ctx, filter, "list", "decode", options.Find().
		SetSort(bson.M{"created_at": -1}).
		SetSkip(int64(p.Offset)).
		SetLimit(int64(ClampLimit(p.Limit))))
}

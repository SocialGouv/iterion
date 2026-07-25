package configshare

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

const (
	colShares          = "config_shares"
	colShareDeliveries = "config_share_deliveries"
	// deliveryTTLDays caps how long the config-share audit rows are retained.
	deliveryTTLDays = 90
)

// MongoStore is the cloud-mode config-share Store (persistent, multi-replica).
type MongoStore struct {
	shares     *mongo.Collection
	deliveries *mongo.Collection
}

// NewMongoStore builds the store over a database.
func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{
		shares:     db.Collection(colShares),
		deliveries: db.Collection(colShareDeliveries),
	}
}

// Ensure MongoStore satisfies Store.
var _ Store = (*MongoStore)(nil)

// EnsureSchema creates the config-share indexes idempotently: a unique token
// hash (the auth lookup), a per-tenant recent index (operator listing), and a
// TTL on the delivery audit rows.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	shares := db.Collection(colShares)
	if _, err := shares.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "token_hash", Value: 1}}, Options: options.Index().SetUnique(true).SetName("token_hash_unique")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("tenant_created")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("configshare: ensure share indexes: %w", err)
	}
	deliveries := db.Collection(colShareDeliveries)
	if _, err := deliveries.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "share_id", Value: 1}, {Key: "at", Value: -1}}, Options: options.Index().SetName("share_recent")},
		{Keys: bson.D{{Key: "at", Value: 1}}, Options: options.Index().SetName("configshare_deliveries_ttl").SetExpireAfterSeconds(int32(deliveryTTLDays * 24 * 60 * 60))},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("configshare: ensure delivery indexes: %w", err)
	}
	return nil
}

func (s *MongoStore) Create(ctx context.Context, sh *Share) error {
	if _, err := s.shares.InsertOne(ctx, sh); err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return fmt.Errorf("configshare: share %q already exists", sh.ID)
		}
		return fmt.Errorf("configshare: insert share: %w", err)
	}
	return nil
}

func (s *MongoStore) GetByID(ctx context.Context, id string) (*Share, error) {
	sh, err := mongoutil.FindOne[Share](ctx, s.shares, bson.M{"_id": id}, ErrNotFound, "configshare: get share")
	if err != nil {
		return nil, err
	}
	return &sh, nil
}

func (s *MongoStore) ListByTenant(ctx context.Context, tenantID string) ([]*Share, error) {
	rows, err := mongoutil.FindAllSorted[Share](ctx, s.shares, bson.M{"tenant_id": tenantID}, "created_at",
		"configshare: list shares", "configshare: decode shares")
	if err != nil {
		return nil, err
	}
	// Newest-first (FindAllSorted is ascending on created_at).
	out := make([]*Share, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		out = append(out, &r)
	}
	return out, nil
}

func (s *MongoStore) Update(ctx context.Context, sh *Share) error {
	return mongoutil.ReplaceOneChecked(ctx, s.shares, bson.M{"_id": sh.ID}, sh, nil, ErrNotFound, "configshare: update share")
}

func (s *MongoStore) Delete(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.shares, bson.M{"_id": id}, ErrNotFound, "configshare: delete share")
}

func (s *MongoStore) Touch(ctx context.Context, id string, at time.Time) error {
	if _, err := s.shares.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"last_used_at": at}}); err != nil {
		return fmt.Errorf("configshare: touch share: %w", err)
	}
	return nil
}

func (s *MongoStore) RecordDelivery(ctx context.Context, d *Delivery) error {
	if _, err := s.deliveries.InsertOne(ctx, d); err != nil {
		return fmt.Errorf("configshare: insert delivery: %w", err)
	}
	return nil
}

func (s *MongoStore) ListDeliveries(ctx context.Context, shareID string, limit int) ([]*Delivery, error) {
	if limit <= 0 {
		limit = 100
	}
	cur, err := s.deliveries.Find(ctx, bson.M{"share_id": shareID},
		options.Find().SetSort(bson.M{"at": -1}).SetLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("configshare: list deliveries: %w", err)
	}
	defer cur.Close(ctx)
	var rows []Delivery
	if err := cur.All(ctx, &rows); err != nil {
		return nil, fmt.Errorf("configshare: decode deliveries: %w", err)
	}
	out := make([]*Delivery, 0, len(rows))
	for i := range rows {
		out = append(out, &rows[i])
	}
	return out, nil
}

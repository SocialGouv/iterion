package webhooks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// Collection names.
const (
	colConfigs    = "webhook_configs"
	colDeliveries = "webhook_deliveries"
	colQuotas     = "webhook_quotas"
)

// DeliveryTTLDays caps how long delivery audit rows are retained.
const DeliveryTTLDays = 90

// MongoStores bundles the three Mongo-backed stores over one database
// (reuse via the cloud store's DB() accessor). Each sub-store satisfies
// one interface; they are split because ConfigStore + DeliveryStore both
// declare an Update method.
type MongoStores struct {
	Configs    *MongoConfigStore
	Deliveries *MongoDeliveryStore
	Counter    *MongoCounter
}

func NewMongoStores(db *mongo.Database) *MongoStores {
	return &MongoStores{
		Configs:    &MongoConfigStore{col: db.Collection(colConfigs)},
		Deliveries: &MongoDeliveryStore{col: db.Collection(colDeliveries)},
		Counter:    &MongoCounter{col: db.Collection(colQuotas)},
	}
}

// EnsureSchema creates every webhook index idempotently.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	configs := db.Collection(colConfigs)
	if _, err := configs.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "token_hash", Value: 1}}, Options: options.Index().SetUnique(true).SetName("token_hash_unique")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("tenant_created")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("webhooks: ensure config indexes: %w", err)
	}
	deliveries := db.Collection(colDeliveries)
	if _, err := deliveries.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "idempotency_key", Value: 1}}, Options: options.Index().SetUnique(true).SetName("idempotency_unique")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "webhook_id", Value: 1}, {Key: "received_at", Value: -1}}, Options: options.Index().SetName("tenant_webhook_recent")},
		{Keys: bson.D{{Key: "received_at", Value: 1}}, Options: options.Index().SetName("deliveries_ttl").SetExpireAfterSeconds(int32(DeliveryTTLDays * 24 * 60 * 60))},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("webhooks: ensure delivery indexes: %w", err)
	}
	return nil
}

// ---- MongoConfigStore ----

type MongoConfigStore struct{ col *mongo.Collection }

func (s *MongoConfigStore) Create(ctx context.Context, c Config) error {
	if _, err := s.col.InsertOne(ctx, c); err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return ErrDuplicate
		}
		return fmt.Errorf("webhooks: insert config: %w", err)
	}
	return nil
}

func (s *MongoConfigStore) Get(ctx context.Context, id string) (Config, error) {
	var c Config
	err := s.col.FindOne(ctx, bson.M{"_id": id}).Decode(&c)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Config{}, ErrNotFound
	}
	if err != nil {
		return Config{}, fmt.Errorf("webhooks: get config: %w", err)
	}
	return c, nil
}

func (s *MongoConfigStore) Update(ctx context.Context, c Config) error {
	res, err := s.col.ReplaceOne(ctx, bson.M{"_id": c.ID}, c)
	if err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return ErrDuplicate
		}
		return fmt.Errorf("webhooks: update config: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoConfigStore) Delete(ctx context.Context, id string) error {
	res, err := s.col.DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return fmt.Errorf("webhooks: delete config: %w", err)
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoConfigStore) ListByTenant(ctx context.Context, tenantID string) ([]Config, error) {
	return mongoutil.FindAllSorted[Config](ctx, s.col, bson.M{"tenant_id": tenantID}, "created_at",
		"webhooks: list configs", "webhooks: decode configs")
}

func (s *MongoConfigStore) MarkUsed(ctx context.Context, id string, t time.Time) error {
	_, err := s.col.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"last_used_at": t}})
	if err != nil {
		return fmt.Errorf("webhooks: mark used: %w", err)
	}
	return nil
}

// ---- MongoDeliveryStore ----

type MongoDeliveryStore struct{ col *mongo.Collection }

func (s *MongoDeliveryStore) Insert(ctx context.Context, d Delivery) error {
	if _, err := s.col.InsertOne(ctx, d); err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return ErrDuplicate
		}
		return fmt.Errorf("webhooks: insert delivery: %w", err)
	}
	return nil
}

func (s *MongoDeliveryStore) GetByIdempotencyKey(ctx context.Context, key string) (Delivery, error) {
	var d Delivery
	err := s.col.FindOne(ctx, bson.M{"idempotency_key": key}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Delivery{}, ErrNotFound
	}
	if err != nil {
		return Delivery{}, fmt.Errorf("webhooks: get delivery: %w", err)
	}
	return d, nil
}

func (s *MongoDeliveryStore) Update(ctx context.Context, d Delivery) error {
	res, err := s.col.ReplaceOne(ctx, bson.M{"_id": d.ID}, d)
	if err != nil {
		return fmt.Errorf("webhooks: update delivery: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoDeliveryStore) ListByWebhook(ctx context.Context, tenantID, webhookID string, limit int) ([]Delivery, error) {
	if limit <= 0 {
		limit = 50
	}
	cur, err := s.col.Find(ctx,
		bson.M{"tenant_id": tenantID, "webhook_id": webhookID},
		options.Find().SetSort(bson.M{"received_at": -1}).SetLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("webhooks: list deliveries: %w", err)
	}
	defer cur.Close(ctx)
	var out []Delivery
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("webhooks: decode deliveries: %w", err)
	}
	return out, nil
}

// ---- MongoCounter ----

type MongoCounter struct{ col *mongo.Collection }

// Allow increments the org (and optional per-webhook) monthly counters
// and rolls back + denies when a cap is breached. Counters are
// eventually consistent under heavy concurrency (a denied call rolls
// back its increment); the allow/deny decision is atomic per
// findOneAndUpdate, which is the property a monthly call cap needs.
func (s *MongoCounter) Allow(ctx context.Context, tenantID, webhookID string, when time.Time, lim Limits) (bool, error) {
	m := monthKey(when)
	orgKey := "org|" + tenantID + "|" + m
	if ok, err := s.bump(ctx, orgKey, lim.PerOrgMonthly); err != nil || !ok {
		return false, err
	}
	if lim.PerWebhookMonthly > 0 {
		whKey := "wh|" + tenantID + "|" + webhookID + "|" + m
		ok, err := s.bump(ctx, whKey, lim.PerWebhookMonthly)
		if err != nil || !ok {
			// Detached ctx so a cancelled request still rolls back the org
			// increment; otherwise the per-org monthly count leaks a unit.
			_, _ = s.col.UpdateOne(context.WithoutCancel(ctx), bson.M{"_id": orgKey}, bson.M{"$inc": bson.M{"count": -1}})
			return false, err
		}
	}
	return true, nil
}

func (s *MongoCounter) bump(ctx context.Context, key string, limit int) (bool, error) {
	var doc struct {
		Count int `bson:"count"`
	}
	err := s.col.FindOneAndUpdate(ctx,
		bson.M{"_id": key},
		bson.M{"$inc": bson.M{"count": 1}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&doc)
	if err != nil {
		return false, fmt.Errorf("webhooks: counter bump: %w", err)
	}
	if limit > 0 && doc.Count > limit {
		_, _ = s.col.UpdateOne(ctx, bson.M{"_id": key}, bson.M{"$inc": bson.M{"count": -1}})
		return false, nil
	}
	return true, nil
}

func (s *MongoCounter) OrgCount(ctx context.Context, tenantID string, when time.Time) (int, error) {
	var doc struct {
		Count int `bson:"count"`
	}
	err := s.col.FindOne(ctx, bson.M{"_id": "org|" + tenantID + "|" + monthKey(when)}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("webhooks: org count: %w", err)
	}
	return doc.Count, nil
}

package oidc

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// StatesCollectionName is the Mongo collection backing PendingAuth records.
const StatesCollectionName = "oidc_states"

// MongoStateStore is a Mongo-backed StateStore. The in-memory store is
// per-process, so an OIDC /start on replica A and /callback on replica B would
// fail "state expired or invalid" — common once per-org Keycloak flows (with
// slower IdP MFA prompts) run behind an autoscaled control plane. This store
// shares PendingAuth across replicas; expired rows are reaped by a Mongo TTL
// index AND re-checked on Take (TTL deletion is lazy, ~60s).
type MongoStateStore struct {
	coll *mongo.Collection
	ttl  time.Duration
}

// mongoStateDoc wraps PendingAuth with the state as _id and an absolute expiry
// for the TTL index.
type mongoStateDoc struct {
	State     string      `bson:"_id"`
	Pending   PendingAuth `bson:"pending"`
	ExpiresAt time.Time   `bson:"expires_at"`
}

// NewMongoStateStore wires a Mongo-backed state store with a per-entry TTL.
func NewMongoStateStore(db *mongo.Database, ttl time.Duration) *MongoStateStore {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &MongoStateStore{coll: db.Collection(StatesCollectionName), ttl: ttl}
}

// EnsureSchema creates the TTL index on expires_at (Mongo evicts a row once
// expires_at is in the past).
func (s *MongoStateStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "expires_at", Value: 1}},
			Options: options.Index().SetName("ttl_expires_at").SetExpireAfterSeconds(0),
		},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("oidc: ensure oidc_states indexes: %w", err)
	}
	return nil
}

// Put persists a PendingAuth keyed by its state (upsert — a regenerated state
// collision, astronomically unlikely, just overwrites).
func (s *MongoStateStore) Put(ctx context.Context, p PendingAuth) error {
	issued := p.IssuedAt
	if issued.IsZero() {
		issued = time.Now()
	}
	doc := mongoStateDoc{State: p.State, Pending: p, ExpiresAt: issued.Add(s.ttl)}
	_, err := s.coll.ReplaceOne(ctx, bson.M{"_id": p.State}, doc, options.Replace().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("oidc: put state: %w", err)
	}
	return nil
}

// Take atomically fetches and deletes the PendingAuth for state (single-use),
// re-checking the TTL in case Mongo hasn't reaped the row yet.
func (s *MongoStateStore) Take(ctx context.Context, state string) (PendingAuth, error) {
	doc, err := mongoutil.FindOneAndDeleteChecked[mongoStateDoc](ctx, s.coll, bson.M{"_id": state}, ErrStateNotFound, "oidc: take state")
	if err != nil {
		return PendingAuth{}, err
	}
	if time.Since(doc.Pending.IssuedAt) > s.ttl {
		return PendingAuth{}, ErrStateNotFound
	}
	return doc.Pending, nil
}

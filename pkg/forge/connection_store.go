package forge

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// ConnectionStore persists forge connections. Get is intentionally NOT
// tenant-scoped (the GitHub-App + OAuth callbacks resolve the tenant from
// signed state, and the refresh worker runs cross-tenant); the HTTP CRUD
// layer asserts tenant ownership before mutating — same contract as
// webhooks.ConfigStore.
type ConnectionStore interface {
	Create(ctx context.Context, c Connection) error
	Get(ctx context.Context, id string) (Connection, error)
	Update(ctx context.Context, c Connection) error
	Delete(ctx context.Context, id string) error
	ListByTenant(ctx context.Context, tenantID string) ([]Connection, error)
	// ExpiringBefore returns refreshable connections whose access token
	// expires at or before t (cross-tenant; the refresh worker's scan).
	// KindPAT connections never expire and are excluded.
	ExpiringBefore(ctx context.Context, t time.Time) ([]Connection, error)
}

// ---- in-memory store (tests / local) ----

type MemoryConnectionStore struct {
	mu    sync.RWMutex
	conns map[string]Connection
}

func NewMemoryConnectionStore() *MemoryConnectionStore {
	return &MemoryConnectionStore{conns: make(map[string]Connection)}
}

func (m *MemoryConnectionStore) Create(_ context.Context, c Connection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conns[c.ID] = c
	return nil
}

func (m *MemoryConnectionStore) Get(_ context.Context, id string) (Connection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.conns[id]
	if !ok {
		return Connection{}, ErrConnectionNotFound
	}
	return c, nil
}

func (m *MemoryConnectionStore) Update(_ context.Context, c Connection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.conns[c.ID]; !ok {
		return ErrConnectionNotFound
	}
	m.conns[c.ID] = c
	return nil
}

func (m *MemoryConnectionStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.conns[id]; !ok {
		return ErrConnectionNotFound
	}
	delete(m.conns, id)
	return nil
}

func (m *MemoryConnectionStore) ListByTenant(_ context.Context, tenantID string) ([]Connection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Connection
	for _, c := range m.conns {
		if c.TenantID == tenantID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemoryConnectionStore) ExpiringBefore(_ context.Context, t time.Time) ([]Connection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Connection
	for _, c := range m.conns {
		if c.Kind == KindPAT || c.AccessTokenExpiresAt == nil {
			continue
		}
		if !c.AccessTokenExpiresAt.After(t) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccessTokenExpiresAt.Before(*out[j].AccessTokenExpiresAt) })
	return out, nil
}

// ---- Mongo store ----

const ConnectionsCollectionName = "forge_connections"

type MongoConnectionStore struct {
	coll *mongo.Collection
}

func NewMongoConnectionStore(db *mongo.Database) *MongoConnectionStore {
	return &MongoConnectionStore{coll: db.Collection(ConnectionsCollectionName)}
}

func (s *MongoConnectionStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "provider", Value: 1}}, Options: options.Index().SetName("tenant_provider")},
		{Keys: bson.D{{Key: "access_token_expires_at", Value: 1}}, Options: options.Index().SetName("token_expiry")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("forge: ensure forge_connections indexes: %w", err)
	}
	return nil
}

func (s *MongoConnectionStore) Create(ctx context.Context, c Connection) error {
	if _, err := s.coll.InsertOne(ctx, c); err != nil {
		return fmt.Errorf("forge: insert connection: %w", err)
	}
	return nil
}

func (s *MongoConnectionStore) Get(ctx context.Context, id string) (Connection, error) {
	return mongoutil.FindOne[Connection](ctx, s.coll, bson.M{"_id": id}, ErrConnectionNotFound, "forge: get connection")
}

func (s *MongoConnectionStore) Update(ctx context.Context, c Connection) error {
	return mongoutil.ReplaceOneChecked(ctx, s.coll, bson.M{"_id": c.ID}, c, nil, ErrConnectionNotFound, "forge: update connection")
}

func (s *MongoConnectionStore) Delete(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll, bson.M{"_id": id}, ErrConnectionNotFound, "forge: delete connection")
}

func (s *MongoConnectionStore) ListByTenant(ctx context.Context, tenantID string) ([]Connection, error) {
	return mongoutil.FindAllSorted[Connection](ctx, s.coll, bson.M{"tenant_id": tenantID}, "created_at",
		"forge: list connections", "forge: decode connections")
}

func (s *MongoConnectionStore) ExpiringBefore(ctx context.Context, t time.Time) ([]Connection, error) {
	filter := bson.M{
		"kind":                    bson.M{"$ne": string(KindPAT)},
		"access_token_expires_at": bson.M{"$lte": t},
	}
	return mongoutil.FindAllSorted[Connection](ctx, s.coll, filter, "access_token_expires_at",
		"forge: list expiring connections", "forge: decode expiring connections")
}

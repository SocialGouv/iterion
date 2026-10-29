package forge

import (
	"bytes"
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
	// RevokeIfPayload flips the row to StatusNeedsReauth — and clears its
	// recorded access-token expiry when clearExpiry — only while its stored
	// sealed payload still equals expected: the compare-and-swap a refresh
	// sweep needs to mark a connection it loaded without writing over a
	// renewal another replica landed since (every renewal changes the
	// payload), and without touching any other field an operator wrote
	// meanwhile (a webhook base pin, a display name, an opt-out). It reports
	// false, nil when the row exists with another payload, and
	// ErrConnectionNotFound when there is no row.
	RevokeIfPayload(ctx context.Context, id string, expected []byte, at time.Time, clearExpiry bool) (bool, error)
	Delete(ctx context.Context, id string) error
	ListByTenant(ctx context.Context, tenantID string) ([]Connection, error)
	// ExpiringBefore returns refreshable connections whose access token
	// expires at or before t (cross-tenant; the refresh worker's scan).
	// KindPAT connections never expire and are excluded. So are
	// StatusNeedsReauth and StatusRevoked rows: their credential is already
	// dead, so handing one to the refresher is a guaranteed invalid_grant
	// against the forge on every tick until an operator re-authorises it —
	// exactly the hot retry loop #2698 names. A StatusDegraded row stays
	// listed: the security-read lane still owes it a withdrawal/park pass.
	ExpiringBefore(ctx context.Context, t time.Time) ([]Connection, error)
	// ListByOAuthApp returns every connection (cross-tenant) that names the
	// OAuth app — the guard that keeps an app in use from being deleted: a
	// platform app serves connections of many teams, and deleting it would
	// silently end every one of their token refreshes.
	ListByOAuthApp(ctx context.Context, appID string) ([]Connection, error)
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

func (m *MemoryConnectionStore) RevokeIfPayload(_ context.Context, id string, expected []byte, at time.Time, clearExpiry bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.conns[id]
	if !ok {
		return false, ErrConnectionNotFound
	}
	if !bytes.Equal(cur.SealedPayload, expected) {
		return false, nil
	}
	cur.Status = StatusNeedsReauth
	cur.UpdatedAt = at
	if clearExpiry {
		cur.AccessTokenExpiresAt = nil
	}
	m.conns[id] = cur
	return true, nil
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

func (m *MemoryConnectionStore) ListByOAuthApp(_ context.Context, appID string) ([]Connection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Connection
	for _, c := range m.conns {
		if appID != "" && c.OAuthAppID == appID {
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
		if !refreshableStatus(c.Status) {
			continue
		}
		if !c.AccessTokenExpiresAt.After(t) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccessTokenExpiresAt.Before(*out[j].AccessTokenExpiresAt) })
	return out, nil
}

// refreshableStatus reports whether the sweep can still help a connection:
// a refresh only has a chance on an active or degraded credential. A
// needs_reauth/revoked row keeps its past expiry, so without the filter it
// is swept on every tick and re-refused by the forge every time (#2698).
// The empty status of rows predating the field stays sweepable.
func refreshableStatus(s ConnectionStatus) bool {
	return s != StatusNeedsReauth && s != StatusRevoked
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

// RevokeIfPayload is one UpdateOne whose filter carries the payload the
// caller loaded and whose update names only the fields a revocation owns:
// the match and the write are the same atomic statement, so a renewal
// landing in between matches nothing, and nothing else on the row is
// rewritten.
func (s *MongoConnectionStore) RevokeIfPayload(ctx context.Context, id string, expected []byte, at time.Time, clearExpiry bool) (bool, error) {
	update := bson.M{"$set": bson.M{"status": StatusNeedsReauth, "updated_at": at}}
	if clearExpiry {
		update["$unset"] = bson.M{"access_token_expires_at": ""}
	}
	res, err := s.coll.UpdateOne(ctx, bson.M{"_id": id, "sealed_payload": expected}, update)
	if err != nil {
		return false, fmt.Errorf("forge: revoke connection %s if its credential is unchanged: %w", id, err)
	}
	if res.MatchedCount > 0 {
		return true, nil
	}
	if _, err := s.Get(ctx, id); err != nil {
		return false, err
	}
	return false, nil
}

func (s *MongoConnectionStore) Delete(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll, bson.M{"_id": id}, ErrConnectionNotFound, "forge: delete connection")
}

func (s *MongoConnectionStore) ListByTenant(ctx context.Context, tenantID string) ([]Connection, error) {
	return mongoutil.FindAllSorted[Connection](ctx, s.coll, bson.M{"tenant_id": tenantID}, "created_at",
		"forge: list connections", "forge: decode connections")
}

func (s *MongoConnectionStore) ListByOAuthApp(ctx context.Context, appID string) ([]Connection, error) {
	if appID == "" {
		// An empty id would match every connection that predates the field.
		return nil, nil
	}
	return mongoutil.FindAllSorted[Connection](ctx, s.coll, bson.M{"oauth_app_id": appID}, "created_at",
		"forge: list connections by oauth app", "forge: decode connections by oauth app")
}

func (s *MongoConnectionStore) ExpiringBefore(ctx context.Context, t time.Time) ([]Connection, error) {
	filter := bson.M{
		"kind":                    bson.M{"$ne": string(KindPAT)},
		"access_token_expires_at": bson.M{"$lte": t},
		// The twin of refreshableStatus: $nin (not $in) so a row predating
		// the status field — no "status" key at all — stays sweepable.
		"status": bson.M{"$nin": bson.A{string(StatusNeedsReauth), string(StatusRevoked)}},
	}
	return mongoutil.FindAllSorted[Connection](ctx, s.coll, filter, "access_token_expires_at",
		"forge: list expiring connections", "forge: decode expiring connections")
}

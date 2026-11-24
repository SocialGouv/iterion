package connection

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// ErrForgeConnectionNotFound reports a forge-connection store miss. It is
// deliberately OUTSIDE the ErrNotFound class for the same reason its forge-era
// twin was: it is iterion's own state, never an answer the forge gave, and
// every handler that %w-wraps a store failure relies on it not classifying as
// a forge 404 (pkg/forge's sentinel block holds the class rule).
var ErrForgeConnectionNotFound = errors.New("forge: connection not found")

// ForgeStore persists forge connections — the store half of the contract
// #2760 moved out of pkg/forge.
//
// The move is where the package doc's first property reaches the forge
// model: EVERY single-record read takes the tenant as a positional argument,
// so a caller cannot forget it — they must produce one, and producing the
// wrong one is a mistake a reviewer can see. Before the contract, `Get`
// filtered by `_id` alone and every caller re-implemented the ownership
// check by hand (`conn.TenantID != want` after the read); the ones that
// forgot — or compared against the wrong field — read across tenants. A
// record that exists but belongs to another tenant answers
// ErrForgeConnectionNotFound, never a distinguishable "forbidden", like the
// generic Store and for the same reason.
//
// Two reads stay CROSS-TENANT, by design and by name:
//
//   - ExpiringBefore is the refresh worker's SCAN. A sweep owes every tenant
//     a pass from one lease, so no single tenant can be its argument; what it
//     returns carries TenantID, and every per-connection action the worker
//     then takes (the RevokeIfPayload CAS, the opt-in re-read) goes through a
//     tenant-scoped signature with THAT tenant.
//   - ListByOAuthApp is the guard that keeps an OAuth app in use from being
//     deleted: a platform app serves connections of many teams, and deleting
//     it would silently end every one of their token refreshes.
type ForgeStore interface {
	Create(ctx context.Context, c ForgeConnection) error
	// Get returns one connection of tenantID. A record belonging to another
	// tenant answers ErrForgeConnectionNotFound — the tenant is in the
	// filter, not checked after the read.
	Get(ctx context.Context, tenantID, id string) (ForgeConnection, error)
	// Update replaces the record, matched on its id AND its TenantID: an
	// update may not move a connection between tenants, which would be a
	// silent grant of one tenant's credential to another written as an
	// ordinary edit.
	Update(ctx context.Context, c ForgeConnection) error
	// RevokeIfPayload flips the row to StatusNeedsReauth — and clears its
	// recorded access-token expiry when clearExpiry — only while its stored
	// sealed payload still equals expected: the compare-and-swap a refresh
	// sweep needs to mark a connection it loaded without writing over a
	// renewal another replica landed since (every renewal changes the
	// payload), and without touching any other field an operator wrote
	// meanwhile (a webhook base pin, a display name, an opt-out). It reports
	// false, nil when the row exists with another payload, and
	// ErrForgeConnectionNotFound when there is no row for this tenant.
	RevokeIfPayload(ctx context.Context, tenantID, id string, expected []byte, at time.Time, clearExpiry bool) (bool, error)
	Delete(ctx context.Context, tenantID, id string) error
	ListByTenant(ctx context.Context, tenantID string) ([]ForgeConnection, error)
	// ExpiringBefore returns refreshable connections whose access token
	// expires at or before t (cross-tenant; the refresh worker's scan — see
	// the interface doc). KindPAT connections never expire and are excluded.
	// So are StatusNeedsReauth and StatusRevoked rows: their credential is
	// already dead, so handing one to the refresher is a guaranteed
	// invalid_grant against the forge on every tick until an operator
	// re-authorises it — exactly the hot retry loop #2698 names. A
	// StatusDegraded row stays listed: the security-read lane still owes it
	// a withdrawal/park pass.
	ExpiringBefore(ctx context.Context, t time.Time) ([]ForgeConnection, error)
	// ListByOAuthApp returns every connection (cross-tenant) that names the
	// OAuth app — the guard that keeps an app in use from being deleted: a
	// platform app serves connections of many teams, and deleting it would
	// silently end every one of their token refreshes.
	ListByOAuthApp(ctx context.Context, appID string) ([]ForgeConnection, error)
}

// ---- in-memory store (tests / local) ----

// MemoryForgeStore is the in-process ForgeStore twin.
type MemoryForgeStore struct {
	mu    sync.RWMutex
	conns map[string]ForgeConnection
}

func NewMemoryForgeStore() *MemoryForgeStore {
	return &MemoryForgeStore{conns: make(map[string]ForgeConnection)}
}

func (m *MemoryForgeStore) Create(_ context.Context, c ForgeConnection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conns[c.ID] = c
	return nil
}

func (m *MemoryForgeStore) Get(_ context.Context, tenantID, id string) (ForgeConnection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.conns[id]
	// The tenant check is the point of the signature. A record that exists
	// but belongs elsewhere is NOT FOUND, not forbidden.
	if !ok || c.TenantID != tenantID {
		return ForgeConnection{}, ErrForgeConnectionNotFound
	}
	return c, nil
}

func (m *MemoryForgeStore) Update(_ context.Context, c ForgeConnection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, ok := m.conns[c.ID]
	// Not found covers both cases: no such id, and an update trying to move
	// the record between tenants.
	if !ok || prev.TenantID != c.TenantID {
		return ErrForgeConnectionNotFound
	}
	m.conns[c.ID] = c
	return nil
}

func (m *MemoryForgeStore) RevokeIfPayload(_ context.Context, tenantID, id string, expected []byte, at time.Time, clearExpiry bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.conns[id]
	if !ok || cur.TenantID != tenantID {
		return false, ErrForgeConnectionNotFound
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

func (m *MemoryForgeStore) Delete(_ context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[id]
	if !ok || c.TenantID != tenantID {
		return ErrForgeConnectionNotFound
	}
	delete(m.conns, id)
	return nil
}

func (m *MemoryForgeStore) ListByTenant(_ context.Context, tenantID string) ([]ForgeConnection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []ForgeConnection
	for _, c := range m.conns {
		if c.TenantID == tenantID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemoryForgeStore) ListByOAuthApp(_ context.Context, appID string) ([]ForgeConnection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []ForgeConnection
	for _, c := range m.conns {
		if appID != "" && c.OAuthAppID == appID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemoryForgeStore) ExpiringBefore(_ context.Context, t time.Time) ([]ForgeConnection, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []ForgeConnection
	for _, c := range m.conns {
		if c.Kind == KindPAT || c.AccessTokenExpiresAt == nil {
			continue
		}
		if !refreshableForgeStatus(c.Status) {
			continue
		}
		if !c.AccessTokenExpiresAt.After(t) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccessTokenExpiresAt.Before(*out[j].AccessTokenExpiresAt) })
	return out, nil
}

// refreshableForgeStatus reports whether the sweep can still help a
// connection: a refresh only has a chance on an active or degraded
// credential. A needs_reauth/revoked row keeps its past expiry, so without
// the filter it is swept on every tick and re-refused by the forge every
// time (#2698). The empty status of rows predating the field stays
// sweepable.
func refreshableForgeStatus(s Status) bool {
	return s != StatusNeedsReauth && s != StatusRevoked
}

// ---- Mongo store ----

// ForgeConnectionsCollectionName is the cloud collection the forge
// connections live in — UNCHANGED by the #2760 contract, which is why the
// move runs no data migration: the records, their ids and the indexes are
// byte-identical, only the code that owns them moved.
const ForgeConnectionsCollectionName = "forge_connections"

// MongoForgeStore is the ForgeStore over MongoDB.
type MongoForgeStore struct {
	coll *mongo.Collection
}

func NewMongoForgeStore(db *mongo.Database) *MongoForgeStore {
	return &MongoForgeStore{coll: db.Collection(ForgeConnectionsCollectionName)}
}

func (s *MongoForgeStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "provider", Value: 1}}, Options: options.Index().SetName("tenant_provider")},
		{Keys: bson.D{{Key: "access_token_expires_at", Value: 1}}, Options: options.Index().SetName("token_expiry")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("forge: ensure forge_connections indexes: %w", err)
	}
	return nil
}

func (s *MongoForgeStore) Create(ctx context.Context, c ForgeConnection) error {
	if _, err := s.coll.InsertOne(ctx, c); err != nil {
		return fmt.Errorf("forge: insert connection: %w", err)
	}
	return nil
}

func (s *MongoForgeStore) Get(ctx context.Context, tenantID, id string) (ForgeConnection, error) {
	// The tenant is IN THE FILTER: a record that exists but belongs elsewhere
	// is not found, never forbidden.
	return mongoutil.FindOne[ForgeConnection](ctx, s.coll, bson.M{"_id": id, "tenant_id": tenantID}, ErrForgeConnectionNotFound, "forge: get connection")
}

func (s *MongoForgeStore) Update(ctx context.Context, c ForgeConnection) error {
	return mongoutil.ReplaceOneChecked(ctx, s.coll, bson.M{"_id": c.ID, "tenant_id": c.TenantID}, c, nil, ErrForgeConnectionNotFound, "forge: update connection")
}

// RevokeIfPayload is one UpdateOne whose filter carries the tenant, the id
// and the payload the caller loaded, and whose update names only the fields a
// revocation owns: the match and the write are the same atomic statement, so
// a renewal landing in between matches nothing, and nothing else on the row
// is rewritten.
func (s *MongoForgeStore) RevokeIfPayload(ctx context.Context, tenantID, id string, expected []byte, at time.Time, clearExpiry bool) (bool, error) {
	update := bson.M{"$set": bson.M{"status": StatusNeedsReauth, "updated_at": at}}
	if clearExpiry {
		update["$unset"] = bson.M{"access_token_expires_at": ""}
	}
	res, err := s.coll.UpdateOne(ctx, bson.M{"_id": id, "tenant_id": tenantID, "sealed_payload": expected}, update)
	if err != nil {
		return false, fmt.Errorf("forge: revoke connection %s if its credential is unchanged: %w", id, err)
	}
	if res.MatchedCount > 0 {
		return true, nil
	}
	if _, err := s.Get(ctx, tenantID, id); err != nil {
		return false, err
	}
	return false, nil
}

func (s *MongoForgeStore) Delete(ctx context.Context, tenantID, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll, bson.M{"_id": id, "tenant_id": tenantID}, ErrForgeConnectionNotFound, "forge: delete connection")
}

func (s *MongoForgeStore) ListByTenant(ctx context.Context, tenantID string) ([]ForgeConnection, error) {
	return mongoutil.FindAllSorted[ForgeConnection](ctx, s.coll, bson.M{"tenant_id": tenantID}, "created_at",
		"forge: list connections", "forge: decode connections")
}

func (s *MongoForgeStore) ListByOAuthApp(ctx context.Context, appID string) ([]ForgeConnection, error) {
	if appID == "" {
		// An empty id would match every connection that predates the field.
		return nil, nil
	}
	return mongoutil.FindAllSorted[ForgeConnection](ctx, s.coll, bson.M{"oauth_app_id": appID}, "created_at",
		"forge: list connections by oauth app", "forge: decode connections by oauth app")
}

func (s *MongoForgeStore) ExpiringBefore(ctx context.Context, t time.Time) ([]ForgeConnection, error) {
	filter := bson.M{
		"kind":                    bson.M{"$ne": string(KindPAT)},
		"access_token_expires_at": bson.M{"$lte": t},
		// The twin of refreshableForgeStatus: $nin (not $in) so a row
		// predating the status field — no "status" key at all — stays
		// sweepable.
		"status": bson.M{"$nin": bson.A{string(StatusNeedsReauth), string(StatusRevoked)}},
	}
	return mongoutil.FindAllSorted[ForgeConnection](ctx, s.coll, filter, "access_token_expires_at",
		"forge: list expiring connections", "forge: decode expiring connections")
}

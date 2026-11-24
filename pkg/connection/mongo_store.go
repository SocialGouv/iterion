package connection

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// The CLOUD twin of the connection store (cloud-native rule 3: new durable
// state ships its cloud twin in the same change — pkg/connection's doc holds
// the tenant-scoped-reads property by construction, and a filesystem-only
// seam would make that property unreachable for the deployment that needs it
// most).
//
// Every answer must be the one MemoryStore and FileStore give, which is what
// the shared conformance suite (store_conformance_test.go) exists to pin:
// the same ErrNotFound for another tenant's record, the same ErrExists for a
// colliding alias, the same kept-credential rule on Update. Each twin
// reaching those answers its own way is how the three would drift.

// ConnectionsCollectionName is the cloud collection holding the records.
const ConnectionsCollectionName = "connections"

// MongoStore is the Store over MongoDB.
type MongoStore struct {
	coll  *mongo.Collection
	clock func() time.Time
}

// NewMongoStore builds the twin over db. Like every store under pkg/, the
// collection is named by the package's own constant at the .Collection site
// itself — the sweep tables and the diagram guard are held to that form.
func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{coll: db.Collection(ConnectionsCollectionName), clock: time.Now}
}

// NewMongoStoreIn builds the twin over a NAMED collection. Tests use it to
// give every build a fresh one: the conformance suite's contract is that each
// store starts empty, which a shared collection cannot promise.
func NewMongoStoreIn(db *mongo.Database, collection string) *MongoStore {
	return &MongoStore{coll: db.Collection(collection), clock: time.Now}
}

// Drop removes the underlying collection. Test cleanup only.
func (s *MongoStore) Drop(ctx context.Context) error { return s.coll.Drop(ctx) }

// WithClock pins the clock, so a test can assert on timestamps.
func (s *MongoStore) WithClock(now func() time.Time) *MongoStore {
	s.clock = now
	return s
}

// EnsureSchema creates the indexes.
//
// The UNIQUE (tenant_id, connector, alias_norm) index is what makes the
// alias rule hold under two racing replicas: the memory and file twins check
// alias-free under a lock they hold, and a cloud deployment has no such
// lock. Without the index, two concurrent Creates of one alias both pass
// their check and two records answer one name — the exact failure the
// update-side check in store.go was written against, now between processes.
func (s *MongoStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys: bson.D{
				{Key: "tenant_id", Value: 1},
				{Key: "connector", Value: 1},
				{Key: "alias_norm", Value: 1},
			},
			Options: options.Index().SetName("tenant_connector_alias").SetUnique(true),
		},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("connection: ensure connections indexes: %w", err)
	}
	return nil
}

// aliasNorm is aliasEq's normalisation as a stored value: the uniqueness
// index can only compare a FIELD, so what the twins compare in code is
// projected onto one here. One function for both, or the index and ByAlias
// would normalise differently.
func aliasNorm(alias string) string {
	return strings.ToLower(strings.TrimSpace(alias))
}

// mongoDoc is the stored shape: the record plus the normalised alias the
// unique index reads. The index field is derived, never trusted from a
// caller — every write recomputes it.
type mongoDoc struct {
	Connection `bson:",inline"`
	AliasNorm  string `bson:"alias_norm"`
}

func (s *MongoStore) Create(ctx context.Context, c Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	now := s.clock()
	c.CreatedAt, c.UpdatedAt = now, now
	_, err := s.coll.InsertOne(ctx, mongoDoc{Connection: c, AliasNorm: aliasNorm(c.Alias)})
	if err != nil {
		if mongoutil.IsDuplicateKey(err) {
			// The same answer for a duplicate _id and for an alias the index
			// refuses: to the caller both are "this connection already exists".
			return ErrExists
		}
		return fmt.Errorf("connection: create: %w", err)
	}
	return nil
}

func (s *MongoStore) Get(ctx context.Context, tenantID, id string) (Connection, error) {
	// The tenant is IN THE FILTER, which is the store.go signature's point
	// carried to the query: a record that exists but belongs elsewhere is not
	// found, never forbidden.
	d, err := mongoutil.FindOne[mongoDoc](ctx, s.coll,
		bson.M{"_id": id, "tenant_id": tenantID},
		ErrNotFound, "connection: get")
	if err != nil {
		return Connection{}, err
	}
	return d.Connection, nil
}

func (s *MongoStore) ByAlias(ctx context.Context, tenantID, connector, alias string) (Connection, error) {
	d, err := mongoutil.FindOne[mongoDoc](ctx, s.coll,
		bson.M{"tenant_id": tenantID, "connector": connector, "alias_norm": aliasNorm(alias)},
		ErrNotFound, "connection: by alias")
	if err != nil {
		return Connection{}, err
	}
	return d.Connection, nil
}

func (s *MongoStore) List(ctx context.Context, tenantID, connector string) ([]Connection, error) {
	filter := bson.M{"tenant_id": tenantID}
	if connector != "" {
		filter["connector"] = connector
	}
	// Oldest first, like both twins; the _id tie-break keeps the order total
	// when two records share a created_at tick.
	cur, err := s.coll.Find(ctx, filter, options.Find().SetSort(bson.D{
		{Key: "created_at", Value: 1},
		{Key: "_id", Value: 1},
	}))
	if err != nil {
		return nil, fmt.Errorf("connection: list: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()
	var out []Connection
	for cur.Next(ctx) {
		var d mongoDoc
		if err := cur.Decode(&d); err != nil {
			return nil, fmt.Errorf("connection: decode: %w", err)
		}
		out = append(out, d.Connection)
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("connection: list: %w", err)
	}
	return out, nil
}

func (s *MongoStore) Update(ctx context.Context, c Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	// The previous record answers the two things an update must not decide:
	// the credential (keptCredential — a record rebuilt from the transport
	// shape carries none, and omission means "unchanged") and the creation
	// time. Read-modify-write rather than a blind replace, exactly like the
	// memory twin under its lock; the alias index is what keeps the write
	// half honest across replicas.
	prev, err := s.Get(ctx, c.TenantID, c.ID)
	if err != nil {
		// Not found covers BOTH cases the replace below would match nothing
		// on: no such id, and — the tenant being part of the filter — an
		// update trying to move the record between tenants.
		return err
	}
	if err := checkUnchanged(prev, c); err != nil {
		return err
	}
	expected := prev.UpdatedAt
	c.SealedPayload = keptCredential(prev, c)
	c.CreatedAt = prev.CreatedAt
	c.UpdatedAt = nextStamp(prev.UpdatedAt, s.clock())
	// The precondition is IN THE FILTER as well as checked above, and that is
	// the half the other twins get from their lock: between the Get and this
	// write another replica can land its own replace, and a compare in Go
	// cannot see it. `updated_at` is the record's own version here — mongo
	// stores it at millisecond precision and Get decodes it back, so the
	// value round-trips exactly.
	//
	// A zero match after a SUCCESSFUL Get is therefore a conflict, never a
	// missing record: the id and the tenant were just proven to match.
	return mongoutil.ReplaceOneChecked(ctx, s.coll,
		bson.M{"_id": c.ID, "tenant_id": c.TenantID, "updated_at": expected},
		mongoDoc{Connection: c, AliasNorm: aliasNorm(c.Alias)},
		ErrExists, ErrConflict, "connection: update")
}

func (s *MongoStore) Delete(ctx context.Context, tenantID, id string) error {
	res, err := s.coll.DeleteOne(ctx, bson.M{"_id": id, "tenant_id": tenantID})
	if err != nil {
		return fmt.Errorf("connection: delete: %w", err)
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

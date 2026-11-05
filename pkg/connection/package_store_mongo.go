package connection

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

// The CLOUD twin of the package store (cloud-native rule 3: new durable state
// ships its cloud twin in the same change). The team and platform tiers are
// rows here; the conformance suite (package_store_test.go) holds both twins
// to the same answers.

// PackagesCollectionName is the cloud collection holding the published
// connector packages.
const PackagesCollectionName = "connector_packages"

// MongoPackageStore is the PackageStore over MongoDB.
type MongoPackageStore struct {
	coll  *mongo.Collection
	clock func() time.Time
}

// NewMongoPackageStore builds the twin over db.
func NewMongoPackageStore(db *mongo.Database) *MongoPackageStore {
	return &MongoPackageStore{coll: db.Collection(PackagesCollectionName), clock: time.Now}
}

// WithClock pins the clock, so a test can assert on timestamps.
func (s *MongoPackageStore) WithClock(now func() time.Time) *MongoPackageStore {
	s.clock = now
	return s
}

// EnsureSchema creates the tenant index the tier's Connectors listing scans.
// The (tenant, connector) uniqueness needs none: it is the _id.
func (s *MongoPackageStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "tenant_id", Value: 1}},
		Options: options.Index().SetName("tenant"),
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("connection: ensure connector_packages indexes: %w", err)
	}
	return nil
}

// packageDoc is the stored shape: the natural key is the (tenant, connector)
// pair carried AS the _id, so a publish is one replace and the pair is unique
// by construction rather than by an index a migration could drop.
type packageDoc struct {
	ID            string `bson:"_id"`
	StoredPackage `bson:",inline"`
}

func (s *MongoPackageStore) Publish(ctx context.Context, pkg StoredPackage) error {
	if err := validateStoredPackage(pkg); err != nil {
		return err
	}
	key := packageKey(pkg.TenantID, pkg.ConnectorID)
	prev, err := s.Get(ctx, pkg.TenantID, pkg.ConnectorID)
	switch {
	case err == nil:
		stampPublish(&pkg, &prev, s.clock())
	case errors.Is(err, ErrPackageNotFound):
		stampPublish(&pkg, nil, s.clock())
	default:
		return fmt.Errorf("connection: read package before publish: %w", err)
	}
	_, err = s.coll.ReplaceOne(ctx, bson.M{"_id": key},
		packageDoc{ID: key, StoredPackage: pkg}, options.Replace().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("connection: publish package %s: %w", pkg.ConnectorID, err)
	}
	return nil
}

func (s *MongoPackageStore) Get(ctx context.Context, tenantID, connectorID string) (StoredPackage, error) {
	d, err := mongoutil.FindOne[packageDoc](ctx, s.coll,
		bson.M{"_id": packageKey(tenantID, connectorID)},
		ErrPackageNotFound, "connection: get package")
	if err != nil {
		return StoredPackage{}, err
	}
	return d.StoredPackage, nil
}

func (s *MongoPackageStore) Retract(ctx context.Context, tenantID, connectorID string) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll,
		bson.M{"_id": packageKey(tenantID, connectorID)},
		ErrPackageNotFound, "connection: retract package")
}

func (s *MongoPackageStore) ListByTenant(ctx context.Context, tenantID string) ([]StoredPackage, error) {
	docs, err := mongoutil.FindAllSorted[packageDoc](ctx, s.coll, bson.M{"tenant_id": tenantID}, "connector_id",
		"connection: list packages", "connection: decode packages")
	if err != nil {
		return nil, err
	}
	out := make([]StoredPackage, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.StoredPackage)
	}
	return out, nil
}

func (s *MongoPackageStore) ListConnectorIDs(ctx context.Context, tenantID string) ([]string, error) {
	cur, err := s.coll.Find(ctx, bson.M{"tenant_id": tenantID}, options.Find().
		SetProjection(bson.M{"connector_id": 1}).
		SetSort(bson.M{"connector_id": 1}))
	if err != nil {
		return nil, fmt.Errorf("connection: list package ids: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()
	var rows []struct {
		ConnectorID string `bson:"connector_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, fmt.Errorf("connection: decode package ids: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ConnectorID)
	}
	return out, nil
}

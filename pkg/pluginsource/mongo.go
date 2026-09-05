package pluginsource

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/store"
)

// CollectionName is the Mongo collection backing plugin sources.
//
// This collection is the whole point of the package: it is the DURABLE
// authority a cloud pod's ephemeral filesystem cannot be. A restarted server
// re-derives its plugin checkouts from here instead of silently losing them.
const CollectionName = "plugin_sources"

// MongoStore is the cloud-mode Store.
type MongoStore struct {
	coll *mongo.Collection
}

func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{coll: db.Collection(CollectionName)}
}

func (s *MongoStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		// (tenant, name) is the operator-facing identity: two sources with the
		// same name would race for the same registry directory. Unique per
		// TEAM, never globally — two orgs may each bring their own
		// "deploy-target", which is exactly the org-scoping this enables.
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "name", Value: 1}}, Options: options.Index().SetUnique(true).SetName("tenant_name_unique")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "enabled", Value: 1}}, Options: options.Index().SetName("tenant_enabled")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("pluginsource: ensure %s indexes: %w", CollectionName, err)
	}
	return nil
}

// withTenantFilter pins every query to the caller's tenant, so a source can
// never be read or mutated across team boundaries.
func withTenantFilter(ctx context.Context, base bson.M) (bson.M, error) {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return nil, ErrTenantMissing
	}
	out := make(bson.M, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out["tenant_id"] = tenantID
	return out, nil
}

func (s *MongoStore) Create(ctx context.Context, ps PluginSource) error {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return ErrTenantMissing
	}
	ps.TenantID = tenantID
	if err := ps.Validate(); err != nil {
		return err
	}
	if ps.ID == "" {
		ps.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	ps.CreatedAt, ps.UpdatedAt = now, now
	if _, err := s.coll.InsertOne(ctx, ps); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return ErrNameConflict
		}
		return fmt.Errorf("pluginsource: insert: %w", err)
	}
	return nil
}

func (s *MongoStore) Get(ctx context.Context, id string) (PluginSource, error) {
	filter, err := withTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return PluginSource{}, err
	}
	var out PluginSource
	if err := s.coll.FindOne(ctx, filter).Decode(&out); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return PluginSource{}, ErrNotFound
		}
		return PluginSource{}, fmt.Errorf("pluginsource: get: %w", err)
	}
	return out, nil
}

func (s *MongoStore) Update(ctx context.Context, ps PluginSource) error {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return ErrTenantMissing
	}
	ps.TenantID = tenantID
	if err := ps.Validate(); err != nil {
		return err
	}
	filter, err := withTenantFilter(ctx, bson.M{"_id": ps.ID})
	if err != nil {
		return err
	}
	ps.UpdatedAt = time.Now().UTC()
	res, err := s.coll.UpdateOne(ctx, filter, bson.M{"$set": bson.M{
		"name":       ps.Name,
		"git_url":    ps.GitURL,
		"ref":        ps.Ref,
		"secret_id":  ps.SecretID,
		"enabled":    ps.Enabled,
		"updated_at": ps.UpdatedAt,
	}})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return ErrNameConflict
		}
		return fmt.Errorf("pluginsource: update: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoStore) Delete(ctx context.Context, id string) error {
	filter, err := withTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	res, err := s.coll.DeleteOne(ctx, filter)
	if err != nil {
		return fmt.Errorf("pluginsource: delete: %w", err)
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkDegraded stamps the source's health readout. Filtered by the explicit
// tenant, never the context one, for the reason list() gives.
func (s *MongoStore) MarkDegraded(ctx context.Context, tenantID, id, reason string) error {
	if tenantID == "" {
		return ErrTenantMissing
	}
	res, err := s.coll.UpdateOne(ctx, bson.M{"_id": id, "tenant_id": tenantID}, bson.M{"$set": bson.M{
		"degraded_reason": reason,
		"degraded_at":     time.Now().UTC(),
	}})
	if err != nil {
		return fmt.Errorf("pluginsource: mark degraded: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoStore) ClearDegraded(ctx context.Context, tenantID, id string) error {
	if tenantID == "" {
		return ErrTenantMissing
	}
	res, err := s.coll.UpdateOne(ctx, bson.M{"_id": id, "tenant_id": tenantID}, bson.M{"$unset": bson.M{
		"degraded_reason": "",
		"degraded_at":     "",
	}})
	if err != nil {
		return fmt.Errorf("pluginsource: clear degraded: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoStore) ListEnabledByTenant(ctx context.Context, tenantID string) ([]PluginSource, error) {
	return s.list(ctx, tenantID, bson.M{"enabled": true})
}

func (s *MongoStore) ListByTenant(ctx context.Context, tenantID string) ([]PluginSource, error) {
	return s.list(ctx, tenantID, bson.M{})
}

// list queries by explicit tenantID rather than the context tenant: the
// publisher resolves a run's sources for the team that owns the RUN, which is
// not always the caller's active team.
func (s *MongoStore) list(ctx context.Context, tenantID string, extra bson.M) ([]PluginSource, error) {
	if tenantID == "" {
		return nil, ErrTenantMissing
	}
	filter := bson.M{"tenant_id": tenantID}
	for k, v := range extra {
		filter[k] = v
	}
	cur, err := s.coll.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("pluginsource: list: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()
	var out []PluginSource
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("pluginsource: decode list: %w", err)
	}
	return out, nil
}

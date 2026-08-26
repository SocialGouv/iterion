package botsource

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

// CollectionName is the Mongo collection backing team-authored bot sources.
// It is the durable authority a cloud pod's ephemeral filesystem cannot be:
// a restarted server serves the same tenant bots because they live here, not
// in the pod's home.
const CollectionName = "bot_sources"

// MongoStore is the cloud-mode Store.
type MongoStore struct {
	coll *mongo.Collection
}

func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{coll: db.Collection(CollectionName)}
}

func (s *MongoStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		// (tenant, slug) is the bot's identity within a team: two sources with
		// the same slug would race for the same launch directory. Unique per
		// TEAM, never globally — two teams may each author their own "reviewer".
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "slug", Value: 1}}, Options: options.Index().SetUnique(true).SetName("tenant_slug_unique")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("botsource: ensure %s indexes: %w", CollectionName, err)
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

func (s *MongoStore) Create(ctx context.Context, bs BotSource) (BotSource, error) {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return BotSource{}, ErrTenantMissing
	}
	bs.TenantID = tenantID
	if err := bs.Validate(); err != nil {
		return BotSource{}, err
	}
	if bs.ID == "" {
		bs.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	bs.CreatedAt, bs.UpdatedAt = now, now
	bs.Version = 1
	if bs.Origin == "" {
		bs.Origin = "tenant"
	}
	if _, err := s.coll.InsertOne(ctx, bs); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return BotSource{}, ErrSlugConflict
		}
		return BotSource{}, fmt.Errorf("botsource: insert: %w", err)
	}
	return bs, nil
}

func (s *MongoStore) Get(ctx context.Context, id string) (BotSource, error) {
	filter, err := withTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return BotSource{}, err
	}
	return s.findOne(ctx, filter)
}

func (s *MongoStore) GetBySlug(ctx context.Context, tenantID, slug string) (BotSource, error) {
	if tenantID == "" {
		return BotSource{}, ErrTenantMissing
	}
	// The ctx tenant marker and the argument must agree: the sentinel-
	// scoping defense ("a platform route can never touch a team's row")
	// leans on scoped contexts, so a read that silently ignored the ctx
	// would erode it the day a caller passes mismatched values.
	if ctxTenant, ok := store.TenantFromContext(ctx); ok && ctxTenant != "" && ctxTenant != tenantID {
		return BotSource{}, fmt.Errorf("botsource: tenant mismatch: ctx=%q arg=%q", ctxTenant, tenantID)
	}
	return s.findOne(ctx, bson.M{"tenant_id": tenantID, "slug": slug})
}

func (s *MongoStore) findOne(ctx context.Context, filter bson.M) (BotSource, error) {
	var out BotSource
	if err := s.coll.FindOne(ctx, filter).Decode(&out); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return BotSource{}, ErrNotFound
		}
		return BotSource{}, fmt.Errorf("botsource: get: %w", err)
	}
	return out, nil
}

// Update replaces a source's content under an optimistic-concurrency guard: when
// bs.Version is non-zero the update matches on it, so a write racing a concurrent
// editor fails with ErrVersionConflict instead of clobbering. The stored version
// is incremented atomically.
func (s *MongoStore) Update(ctx context.Context, bs BotSource) (BotSource, error) {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return BotSource{}, ErrTenantMissing
	}
	bs.TenantID = tenantID
	if err := bs.Validate(); err != nil {
		return BotSource{}, err
	}
	base := bson.M{"_id": bs.ID}
	if bs.Version != 0 {
		base["version"] = bs.Version
	}
	filter, err := withTenantFilter(ctx, base)
	if err != nil {
		return BotSource{}, err
	}
	bs.UpdatedAt = time.Now().UTC()
	res, err := s.coll.UpdateOne(ctx, filter, bson.M{
		"$set": bson.M{
			"slug":       bs.Slug,
			"files":      bs.Files,
			"origin":     bs.Origin,
			"updated_by": bs.UpdatedBy,
			"updated_at": bs.UpdatedAt,
		},
		"$inc": bson.M{"version": 1},
	})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return BotSource{}, ErrSlugConflict
		}
		return BotSource{}, fmt.Errorf("botsource: update: %w", err)
	}
	if res.MatchedCount == 0 {
		// Either the id does not exist, or the version guard rejected it. Probe
		// to return the precise error.
		if _, gerr := s.Get(ctx, bs.ID); errors.Is(gerr, ErrNotFound) {
			return BotSource{}, ErrNotFound
		}
		return BotSource{}, ErrVersionConflict
	}
	return s.Get(ctx, bs.ID)
}

func (s *MongoStore) Delete(ctx context.Context, id string) error {
	filter, err := withTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	res, err := s.coll.DeleteOne(ctx, filter)
	if err != nil {
		return fmt.Errorf("botsource: delete: %w", err)
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoStore) ListByTenant(ctx context.Context, tenantID string) ([]BotSource, error) {
	if ctxTenant, ok := store.TenantFromContext(ctx); ok && ctxTenant != "" && ctxTenant != tenantID {
		return nil, fmt.Errorf("botsource: tenant mismatch: ctx=%q arg=%q", ctxTenant, tenantID)
	}
	if tenantID == "" {
		return nil, ErrTenantMissing
	}
	cur, err := s.coll.Find(ctx, bson.M{"tenant_id": tenantID}, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("botsource: list: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()
	var out []BotSource
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("botsource: decode list: %w", err)
	}
	return out, nil
}

package modelprefs

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// CollectionName backs the cloud-mode Store.
const CollectionName = "model_prefs"

// MongoStore is the cloud-mode Store. Rows are keyed on (tenant, user, key) so
// the same operator carries a different choice per team — the tenancy boundary
// every other cloud store keys on.
type MongoStore struct {
	coll *mongo.Collection
}

func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{coll: db.Collection(CollectionName)}
}

func (s *MongoStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "tenant_id", Value: 1},
			{Key: "user_id", Value: 1},
			{Key: "key", Value: 1},
		},
		Options: options.Index().SetName("tenant_user_key").SetUnique(true),
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("modelprefs: ensure indexes: %w", err)
	}
	return nil
}

func (s *MongoStore) filter(tenantID, userID, key string) bson.M {
	return bson.M{"tenant_id": tenantID, "user_id": userID, "key": key}
}

func (s *MongoStore) Get(ctx context.Context, tenantID, userID, key string) (*Pref, error) {
	k, err := NormalizeKey(key)
	if err != nil {
		return nil, err
	}
	var p Pref
	if err := s.coll.FindOne(ctx, s.filter(tenantID, userID, k)).Decode(&p); err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, fmt.Errorf("modelprefs: get: %w", err)
	}
	return &p, nil
}

func (s *MongoStore) Set(ctx context.Context, p *Pref) error {
	k, err := NormalizeKey(p.Key)
	if err != nil {
		return err
	}
	// $set every dimension, including the empty ones: clearing a choice is
	// how an operator returns to the bot's default, so an omitted field must
	// erase the stored value rather than silently keep the old one.
	_, err = s.coll.UpdateOne(ctx,
		s.filter(p.TenantID, p.UserID, k),
		bson.M{"$set": bson.M{
			"model":      p.Model,
			"backend":    p.Backend,
			"effort":     p.Effort,
			"updated_at": nowUTC(),
		}},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("modelprefs: set: %w", err)
	}
	return nil
}

func (s *MongoStore) Delete(ctx context.Context, tenantID, userID, key string) error {
	k, err := NormalizeKey(key)
	if err != nil {
		return err
	}
	if _, err := s.coll.DeleteOne(ctx, s.filter(tenantID, userID, k)); err != nil {
		return fmt.Errorf("modelprefs: delete: %w", err)
	}
	return nil
}

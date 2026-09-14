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
	// A fixed slot per preference makes the cardinality bound atomic across
	// API replicas without transactions or a racy CountDocuments check. The
	// partial index excludes legacy rows until backfillSlots assigns them.
	_, err = s.coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "tenant_id", Value: 1},
			{Key: "user_id", Value: 1},
			{Key: "slot", Value: 1},
		},
		Options: options.Index().SetName("tenant_user_slot").SetUnique(true).
			SetPartialFilterExpression(bson.M{"slot": bson.M{"$type": "int"}}),
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("modelprefs: ensure slot index: %w", err)
	}
	return s.backfillSlots(ctx)
}

// backfillSlots gives pre-cap rows a slot. Multiple replicas may run this at
// once: the per-row "slot still absent" filter and unique slot index make the
// race harmless. A legacy scope already above the cap keeps its excess rows
// readable/updateable but receives no new slots, so it cannot grow further.
func (s *MongoStore) backfillSlots(ctx context.Context) error {
	cur, err := s.coll.Find(ctx, bson.M{"slot": bson.M{"$exists": false}}, options.Find().SetSort(bson.D{
		{Key: "tenant_id", Value: 1}, {Key: "user_id", Value: 1}, {Key: "key", Value: 1},
	}))
	if err != nil {
		return fmt.Errorf("modelprefs: list legacy rows for slot backfill: %w", err)
	}
	defer cur.Close(ctx)
	var rows []struct {
		ID bson.ObjectID `bson:"_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return fmt.Errorf("modelprefs: decode legacy rows for slot backfill: %w", err)
	}
	for _, row := range rows {
		for slot := 0; slot < MaxPreferencesPerScope; slot++ {
			res, uerr := s.coll.UpdateOne(ctx,
				bson.M{"_id": row.ID, "slot": bson.M{"$exists": false}},
				bson.M{"$set": bson.M{"slot": slot}},
			)
			if uerr == nil {
				if res.MatchedCount > 0 {
					break
				}
				break // another replica already assigned this row
			}
			if mongo.IsDuplicateKeyError(uerr) {
				continue
			}
			return fmt.Errorf("modelprefs: backfill slot: %w", uerr)
		}
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
	// Update an existing row first. This path remains available at the cap.
	set := bson.M{"$set": bson.M{
		"model": p.Model, "backend": p.Backend, "effort": p.Effort, "updated_at": nowUTC(),
	}}
	res, err := s.coll.UpdateOne(ctx,
		s.filter(p.TenantID, p.UserID, k),
		set,
	)
	if err != nil {
		return fmt.Errorf("modelprefs: set: %w", err)
	}
	if res.MatchedCount > 0 {
		return nil
	}

	// New rows claim one of a fixed number of unique owner slots. This is the
	// cross-replica enforcement boundary: at most MaxPreferencesPerScope
	// inserts can succeed for one (tenant,user), regardless of request races.
	for slot := 0; slot < MaxPreferencesPerScope; slot++ {
		_, err = s.coll.InsertOne(ctx, bson.M{
			"tenant_id": p.TenantID, "user_id": p.UserID, "key": k, "slot": slot,
			"model": p.Model, "backend": p.Backend, "effort": p.Effort, "updated_at": nowUTC(),
		})
		if err == nil {
			return nil
		}
		if !mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("modelprefs: insert: %w", err)
		}
		// The duplicate may be the SAME key inserted concurrently rather than
		// an occupied slot. In that case apply this caller's latest value.
		if updated, uerr := s.coll.UpdateOne(ctx, s.filter(p.TenantID, p.UserID, k), set); uerr != nil {
			return fmt.Errorf("modelprefs: update after concurrent insert: %w", uerr)
		} else if updated.MatchedCount > 0 {
			return nil
		}
	}
	return fmt.Errorf("%w: maximum %d keys per tenant/user", ErrTooManyPreferences, MaxPreferencesPerScope)
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

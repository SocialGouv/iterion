package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// RecordRetryFailure atomically advances the tenant/workflow circuit's
// failure streak and opens it when the new count reaches threshold. Both
// changes live in one update pipeline: a concurrent RecordRetrySuccess can
// therefore happen wholly before or wholly after this failure, never between
// the increment and a follow-up open write.
func (s *Store) RecordRetryFailure(ctx context.Context, key, runID string, now time.Time, threshold int, cooldown time.Duration) (*store.RetryCircuitState, error) {
	if key == "" {
		return nil, errors.New("retry circuit: empty key")
	}
	if threshold <= 0 {
		threshold = 3
	}
	if cooldown <= 0 {
		cooldown = 15 * time.Minute
	}
	now = now.UTC()
	base := bson.M{"key": key}
	filter := withTenantFilter(ctx, base)
	firstSet := bson.M{
		"key":                  key,
		"last_failure_at":      now,
		"last_failure_run_id":  runID,
		"updated_at":           now,
		"consecutive_failures": bson.M{"$add": bson.A{bson.M{"$ifNull": bson.A{"$consecutive_failures", 0}}, 1}},
	}
	if tenant, ok := store.TenantFromContext(ctx); ok && tenant != "" {
		firstSet["tenant_id"] = tenant
	}
	openUntil := now.Add(cooldown)
	openExpr := bson.M{"$cond": bson.A{
		bson.M{"$gte": bson.A{"$consecutive_failures", threshold}},
		bson.M{"$cond": bson.A{
			bson.M{"$gt": bson.A{bson.M{"$ifNull": bson.A{"$open_until", now}}, openUntil}},
			"$open_until",
			openUntil,
		}},
		bson.M{"$ifNull": bson.A{"$open_until", "$$REMOVE"}},
	}}
	update := mongo.Pipeline{
		bson.D{{Key: "$set", Value: firstSet}},
		bson.D{{Key: "$set", Value: bson.M{"open_until": openExpr}}},
	}
	var state store.RetryCircuitState
	if err := s.retryCircuits.FindOneAndUpdate(ctx, filter, update,
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&state); err != nil {
		return nil, err
	}
	return &state, nil
}

// RetryCircuitOpen reads the durable breaker state. Missing state means the
// circuit is closed and is represented by a nil pointer.
func (s *Store) RetryCircuitOpen(ctx context.Context, key string, now time.Time) (*store.RetryCircuitState, error) {
	if key == "" {
		return nil, nil
	}
	var state store.RetryCircuitState
	err := s.retryCircuits.FindOne(ctx, withTenantFilter(ctx, bson.M{"key": key})).Decode(&state)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if state.OpenUntil == nil || !state.OpenUntil.After(now.UTC()) {
		return nil, nil
	}
	return &state, nil
}

// RecordRetrySuccess closes the shared circuit after a successful run. A
// missing document is harmless: the run may be the first successful attempt
// for this workflow revision.
func (s *Store) RecordRetrySuccess(ctx context.Context, key string, now time.Time) error {
	if key == "" {
		return nil
	}
	_, err := s.retryCircuits.UpdateOne(ctx, withTenantFilter(ctx, bson.M{"key": key}), bson.M{
		"$set":   bson.M{"consecutive_failures": 0, "updated_at": now.UTC()},
		"$unset": bson.M{"open_until": "", "last_failure_at": "", "last_failure_run_id": ""},
	})
	return err
}

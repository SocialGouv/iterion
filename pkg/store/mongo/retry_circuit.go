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
// failure streak. The follow-up $max preserves a longer cooldown already
// opened by a concurrent runner. The operation is intentionally small and
// idempotent at the document boundary: a duplicate delivery may add one
// failure, but it cannot create a second circuit document or lose an open
// interval.
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
	setOnInsert := bson.M{"key": key}
	if tenant, ok := store.TenantFromContext(ctx); ok && tenant != "" {
		setOnInsert["tenant_id"] = tenant
	}
	update := bson.M{
		"$set": bson.M{
			"last_failure_at":     now,
			"last_failure_run_id": runID,
			"updated_at":          now,
		},
		"$inc":         bson.M{"consecutive_failures": 1},
		"$setOnInsert": setOnInsert,
	}
	var state store.RetryCircuitState
	if err := s.retryCircuits.FindOneAndUpdate(ctx, filter, update,
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&state); err != nil {
		return nil, err
	}
	if state.ConsecutiveFailures >= threshold {
		openUntil := now.Add(cooldown)
		if _, err := s.retryCircuits.UpdateOne(ctx, filter, bson.M{
			"$max": bson.M{"open_until": openUntil},
			"$set": bson.M{"updated_at": now},
		}); err != nil {
			return nil, err
		}
		if err := s.retryCircuits.FindOne(ctx, filter).Decode(&state); err != nil {
			return nil, err
		}
	}
	return &state, nil
}

// RetryCircuitOpen reports whether the breaker is OPEN at now, returning its
// state when it is and nil when it is not.
//
// Nil IS the contract: the caller reads this as `if state != nil { the wave
// is blocked }`, so every not-open shape has to collapse to nil — a missing
// document (never opened), a document whose streak has not reached the
// threshold (no open_until at all), and a document whose cooldown has
// EXPIRED. That last one is the trap: the document outlives its own cooldown
// (RecordRetrySuccess is the only thing that removes open_until), so
// returning it non-nil would read a recovered provider as blocked forever.
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

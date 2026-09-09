package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Defensive floors for a caller that passes a non-positive bound. They are
// NOT the product defaults — retrycoord.FromEnv owns those and always hands
// down positive values; these only keep the pipeline arithmetic total if a
// future caller forgets. Kept as local constants rather than importing
// retrycoord, which would point a store implementation at a policy package.
const (
	retrycircuitDefaultThreshold = 3
	retrycircuitDefaultCooldown  = 15 * time.Minute
)

// RecordRetryFailure advances the tenant/workflow circuit's failure streak
// and opens the breaker once the streak reaches threshold, in ONE atomic
// document write.
//
// It is one write because it has to be. The obvious three-call shape —
// $inc, then a conditional $max open_until, then a re-read — is a lost
// update across runner pods: a RecordRetrySuccess landing between the
// increment and the open zeroes the streak and unsets open_until, and the
// racing failure then re-opens a full cooldown on top of a streak that was
// just cleared, delaying every later retry of that revision for nothing. The
// trailing read compounds it by returning whatever a third writer left. An
// aggregation-pipeline update makes the read-decide-write one document
// operation, and options.After returns the state this call actually
// committed.
//
// The streak also DECAYS. It is only ever incremented, and the sole reset is
// RecordRetrySuccess — which needs a successful engine run of the same
// workflow revision. A revision that never produces one (a legitimate
// FailNode terminus, or one whose failures are all usage-window) could never
// clear its streak, so a single storm months ago would arm the breaker on
// the next isolated failure, forever. Stage 1 therefore restarts the count
// at 1 when the previous failure is older than the cooldown: the cooldown is
// the breaker's own notion of how long a storm lasts, so a gap wider than it
// means the previous storm is over. This is also what keeps the threshold
// meaning what Config.Threshold says it means — failures ACROSS runs, close
// together — rather than one lonely run's retries, which are floored minutes
// to hours apart, adding up over a week.
func (s *Store) RecordRetryFailure(ctx context.Context, key, runID string, now time.Time, threshold int, cooldown time.Duration) (*store.RetryCircuitState, error) {
	if key == "" {
		return nil, errors.New("retry circuit: empty key")
	}
	if threshold <= 0 {
		threshold = retrycircuitDefaultThreshold
	}
	if cooldown <= 0 {
		cooldown = retrycircuitDefaultCooldown
	}
	now = now.UTC()
	filter := withTenantFilter(ctx, bson.M{"key": key})
	openUntil := now.Add(cooldown)
	// Failures older than this belong to a storm that has already ended.
	decayBefore := now.Add(-cooldown)

	// Two stages, not one: within a single $set the right-hand side reads the
	// PRE-stage document, so stage 2 could not see the streak stage 1 just
	// wrote. $setOnInsert has no pipeline equivalent either — key/tenant_id
	// are the document's identity, so re-setting them every time is a no-op.
	streak := bson.M{
		"consecutive_failures": bson.M{"$cond": bson.A{
			// $ifNull guards the first failure and the post-success document
			// alike: with no last_failure_at, `now < now-cooldown` is false
			// and the else arm starts the streak at 1.
			bson.M{"$lt": bson.A{bson.M{"$ifNull": bson.A{"$last_failure_at", now}}, decayBefore}},
			1,
			bson.M{"$add": bson.A{bson.M{"$ifNull": bson.A{"$consecutive_failures", 0}}, 1}},
		}},
		"last_failure_at":     now,
		"last_failure_run_id": runID,
		"updated_at":          now,
		"key":                 key,
	}
	if tenant, ok := store.TenantFromContext(ctx); ok && tenant != "" {
		streak["tenant_id"] = tenant
	}
	pipeline := []bson.M{
		{"$set": streak},
		{"$set": bson.M{
			"open_until": bson.M{"$cond": bson.A{
				bson.M{"$gte": bson.A{"$consecutive_failures", threshold}},
				// Never shorten a longer cooldown a concurrent pod already
				// opened. Both arms of $max are non-null dates so the
				// comparison cannot depend on how $max treats a missing field.
				bson.M{"$max": bson.A{bson.M{"$ifNull": bson.A{"$open_until", openUntil}}, openUntil}},
				// Below the threshold: preserve whatever is there, including
				// nothing. $$REMOVE is the explicit "leave the field absent".
				bson.M{"$ifNull": bson.A{"$open_until", "$$REMOVE"}},
			}},
		}},
	}

	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
	var state store.RetryCircuitState
	err := s.retryCircuits.FindOneAndUpdate(ctx, filter, pipeline, opts).Decode(&state)
	if mongoutil.IsDuplicateKey(err) {
		// Two pods recording the FIRST failure of a key race to insert, and
		// the unique {tenant_id, key} index lets exactly one win. The loser's
		// document now exists, so the same call succeeds as a plain update.
		// (Without the index both would insert and the breaker would silently
		// split in two, which is why the retry lives here and not in a
		// looser index.)
		err = s.retryCircuits.FindOneAndUpdate(ctx, filter, pipeline, opts).Decode(&state)
	}
	if err != nil {
		return nil, err
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

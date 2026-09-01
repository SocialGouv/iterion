package mongo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Mongo half of store.RunRetryStore: arming and claiming the durable
// retry a usage-window failure needs.
//
// The wait cannot live in the work queue. That stream is a work queue with
// a 24h max age and exposes no delayed-nak, while a weekly quota window can
// reset up to seven days out — so blind redelivery just burns one pod per
// attempt against a wall that cannot move (measured: eight pods per run,
// then a DLQ park). Mongo is the only place a multi-day intent survives,
// and a periodic sweeper is the only thing that can act on it.

// retryStateField is the run document's embedded retry-state path. Kept in
// one constant because every filter and update below has to agree on it.
const retryStateField = "retry_state"

// retryClaimLease bounds how long a claimed-but-not-resumed retry stays
// off-limits. The claim is a LEASE, not a delete: if it disarmed the retry
// outright, a server pod dying between the claim and the resume would strand
// the run forever — retry_after gone, so no scan ever lists it again, and
// nothing else reconciles a claimed-but-idle row. With a lease, that run
// simply becomes claimable again once the lease lapses.
const retryClaimLease = 10 * time.Minute

func retryPath(field string) string { return retryStateField + "." + field }

// ScheduleRunRetry arms a retry, conditioning on the run still being
// failed_resumable and under its attempt budget. Both preconditions are in
// the FILTER rather than checked first and written second: an operator
// resuming the run by hand between the two would otherwise have their
// resume silently re-armed.
func (s *Store) ScheduleRunRetry(ctx context.Context, runID string, at time.Time, reason, code string, maxAttempts int) (bool, int, error) {
	if maxAttempts <= 0 {
		return false, 0, nil
	}
	now := time.Now().UTC()
	filter := withTenantFilter(ctx, bson.M{
		"_id":    runID,
		"status": string(store.RunStatusFailedResumable),
		// The absent case needs its own branch: Mongo's comparison
		// operators are type-bracketed, so {$lt: n} does NOT match a
		// document where the field is missing. A run that never retried has
		// no retry_state.attempts, so a bare $lt would refuse to arm the
		// FIRST retry of every run — silently disabling the whole feature.
		"$or": []bson.M{
			{retryPath("attempts"): bson.M{"$exists": false}},
			{retryPath("attempts"): bson.M{"$lt": maxAttempts}},
		},
	})
	update := bson.M{
		"$set": bson.M{
			retryPath("retry_after"): at.UTC(),
			retryPath("reason"):      reason,
			retryPath("code"):        code,
			// Arming IS the promotion: continuation_state must only say
			// retry_armed once a retry actually exists (the block point
			// stamps unknown), and this write is the one that creates it.
			"continuation_state": store.ContinuationRetryArmed,
			"updated_at":         now,
		},
		"$unset": bson.M{
			// A fresh arming supersedes the previous failure note and claim.
			retryPath("last_error"): "",
			retryPath("claimed_at"): "",
		},
		"$inc": bson.M{retryPath("attempts"): 1, "version": 1},
	}
	var updated struct {
		RetryState *store.RunRetryState `bson:"retry_state"`
	}
	err := s.runs.FindOneAndUpdate(ctx, filter, update,
		options.FindOneAndUpdate().
			SetReturnDocument(options.After).
			SetProjection(bson.M{retryStateField: 1}),
	).Decode(&updated)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			// Not failed_resumable any more, or out of attempts. Both are
			// ordinary outcomes the caller reports rather than errors.
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("store/mongo: schedule retry %s: %w", runID, err)
	}
	attempt := 0
	if updated.RetryState != nil {
		attempt = updated.RetryState.Attempts
	}
	return true, attempt, nil
}

// ClaimRunRetry leases an armed retry, conditioning on the retry_after value
// the caller read AND on no live lease existing. First writer wins; every
// other replica sees won=false — no leader election.
//
// It deliberately LEAVES retry_after in place. Unsetting it here would make
// the claim exclusive too, but a pod dying before the resume landed would
// then strand the run permanently. Keeping it means the worst case is a
// retry that fires one lease later. The successful resume is what clears it
// (ClearRunRetry).
func (s *Store) ClaimRunRetry(ctx context.Context, runID string, expectedAfter time.Time) (bool, error) {
	now := time.Now().UTC()
	filter := withTenantFilter(ctx, bson.M{
		"_id":                    runID,
		"status":                 string(store.RunStatusFailedResumable),
		retryPath("retry_after"): expectedAfter.UTC(),
		"$or": []bson.M{
			{retryPath("claimed_at"): bson.M{"$exists": false}},
			{retryPath("claimed_at"): bson.M{"$lt": now.Add(-retryClaimLease)}},
		},
	})
	update := bson.M{
		"$set": bson.M{
			retryPath("claimed_at"): now,
			"updated_at":            now,
		},
		"$inc": bson.M{"version": 1},
	}
	res, err := s.runs.UpdateOne(ctx, filter, update)
	if err != nil {
		return false, fmt.Errorf("store/mongo: claim retry %s: %w", runID, err)
	}
	return res.MatchedCount > 0, nil
}

// ClearRunRetry disarms a retry that has been acted on — the successful
// resume's counterpart to the lease taken by ClaimRunRetry. Without it a
// retry_after in the past would survive on the row and re-fire the moment
// the resumed run failed again for an unrelated reason.
func (s *Store) ClearRunRetry(ctx context.Context, runID string) error {
	update := bson.M{
		"$unset": bson.M{retryPath("retry_after"): "", retryPath("claimed_at"): ""},
		"$set":   bson.M{"updated_at": time.Now().UTC()},
		"$inc":   bson.M{"version": 1},
	}
	if _, err := s.runs.UpdateOne(ctx, withTenantFilter(ctx, bson.M{"_id": runID}), update); err != nil {
		return fmt.Errorf("store/mongo: clear retry %s: %w", runID, err)
	}
	return nil
}

// AbandonRunRetry records why a run stopped retrying and leaves it
// disarmed. Not conditioned on status: by the time we abandon, the reason
// is already permanent, and the note matters more than the race.
func (s *Store) AbandonRunRetry(ctx context.Context, runID, reason string) error {
	now := time.Now().UTC()
	update := bson.M{
		"$set": bson.M{
			retryPath("last_error"): reason,
			// Nothing is armed any more — the run's future belongs to a
			// human/consumer decision. Leaving retry_armed here would
			// make an outcome consumer wait forever on a retry that
			// will never fire.
			"continuation_state": store.ContinuationFinal,
			"updated_at":         now,
		},
		"$unset": bson.M{retryPath("retry_after"): ""},
		"$inc":   bson.M{"version": 1},
	}
	if _, err := s.runs.UpdateOne(ctx, withTenantFilter(ctx, bson.M{"_id": runID}), update); err != nil {
		return fmt.Errorf("store/mongo: abandon retry %s: %w", runID, err)
	}
	return nil
}

// RetryDueRef is one row of ListRunsDueForRetry: the run identity plus what
// the sweeper needs to re-enqueue a resume without a second read.
type RetryDueRef struct {
	ID       string `bson:"_id"`
	TenantID string `bson:"tenant_id"`
	OwnerID  string `bson:"owner_id"`
	BotID    string `bson:"bot_id"`
	FilePath string `bson:"file_path"`
	// BotSourceTenant mirrors Run.BotSourceTenant so the sweeper's resume
	// re-resolves the same stored-bot tier the launch used.
	BotSourceTenant string               `bson:"bot_source_tenant"`
	RetryState      *store.RunRetryState `bson:"retry_state"`
}

// RetryAfter returns the armed instant, or the zero time when absent.
func (r RetryDueRef) RetryAfter() time.Time {
	if r.RetryState == nil || r.RetryState.RetryAfter == nil {
		return time.Time{}
	}
	return *r.RetryState.RetryAfter
}

// ListRunsDueForRetry returns runs whose armed retry instant has arrived,
// oldest first. Platform-scoped (the sweeper runs outside any tenant
// context), so callers pass store.WithoutTenantFilter — mirroring
// ListStaleActiveRuns.
func (s *Store) ListRunsDueForRetry(ctx context.Context, before time.Time, limit int) ([]RetryDueRef, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	cur, err := s.runs.Find(ctx,
		withTenantFilter(ctx, bson.M{
			"status":                 string(store.RunStatusFailedResumable),
			retryPath("retry_after"): bson.M{"$lte": before.UTC()},
		}),
		options.Find().
			SetProjection(bson.M{"_id": 1, "tenant_id": 1, "owner_id": 1, "bot_id": 1, "bot_source_tenant": 1, "file_path": 1, retryStateField: 1}).
			SetSort(bson.M{retryPath("retry_after"): 1}).
			SetLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list runs due for retry: %w", err)
	}
	defer cur.Close(ctx)
	var out []RetryDueRef
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("store/mongo: decode runs due for retry: %w", err)
	}
	return out, nil
}

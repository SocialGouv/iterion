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

// wallMoves is the pipeline predicate "this arming MOVES the wall the row is
// waiting on": the row has one, and it is not this instant.
//
// A row with no stored wall answers FALSE. It is the shape armed before this
// field existed, and the probe ledger it carries is the ledger of the wait it
// is ALREADY on — probe_count predates the wall, so a spent budget is a
// reachable state there. Treating the first arming after the upgrade as a
// wall MOVE handed such a row a fresh budget of pods for a window whose
// probes it had already spent.
//
// A single expression shared by the two armings: the probe budget's whole
// bound rests on the two of them answering this question identically, and a
// second copy of a $cond is where that drifts.
func wallMoves(wall time.Time) bson.M {
	return bson.M{"$and": bson.A{
		bson.M{"$ne": bson.A{bson.M{"$type": "$" + retryPath("wall")}, "missing"}},
		bson.M{"$ne": bson.A{"$" + retryPath("wall"), wall.UTC()}},
	}}
}

// probeCountForWall renders probe_count: the BUDGET belongs to the wall, so
// it survives an arming that re-states the wall the row is on and starts over
// when the wall moves.
//
// Only probe_count. last_probe_at is the PACE, and the pace belongs to the
// ROW: no arming may clear it. An arming that cleared it made the row
// probe-eligible on the sweeper's very next pass instead of one floor later
// (ListRunsForRetryProbe's pace term has an "absent" branch), so a provider
// whose refusal re-dates its window on every reading — the window-DURATION
// shape, whose instant is recomputed from now — was pulled once a minute,
// free of attempts, with nothing left to bound it.
func probeCountForWall(wall time.Time) bson.M {
	return bson.M{"$cond": bson.M{
		"if":   wallMoves(wall),
		"then": "$$REMOVE",
		"else": "$" + retryPath("probe_count"),
	}}
}

// ScheduleRunRetry arms a retry, conditioning on the run still being
// failed_resumable and under its attempt budget. Both preconditions are in
// the FILTER rather than checked first and written second: an operator
// resuming the run by hand between the two would otherwise have their
// resume silently re-armed.
//
// An aggregation pipeline rather than a plain update document, because the
// probe ledger's fate depends on the row's OWN stored wall: read it in the
// caller and the sweeper's pull lands in between, handing one wall a second
// budget on a stale comparison.
func (s *Store) ScheduleRunRetry(ctx context.Context, runID string, at, wall time.Time, reason, code string, maxAttempts int) (bool, int, error) {
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
	set := bson.M{
		retryPath("retry_after"):  at.UTC(),
		retryPath("scheduled_at"): now,
		retryPath("reason"):       reason,
		retryPath("code"):         code,
		// $inc's pipeline spelling, with the absent case the filter above
		// documents: a row that never retried has no attempts at all.
		retryPath("attempts"): bson.M{"$add": bson.A{
			bson.M{"$ifNull": bson.A{"$" + retryPath("attempts"), 0}}, 1,
		}},
		// A fresh arming supersedes the previous failure note and claim.
		retryPath("last_error"): "$$REMOVE",
		retryPath("claimed_at"): "$$REMOVE",
		// The marker is CONSUMED by every arming — that is what makes the
		// park after a probe charged normally.
		retryPath("probe_from"): "$$REMOVE",
		// Arming IS the promotion: continuation_state must only say
		// retry_armed once a retry actually exists (the block point
		// stamps unknown), and this write is the one that creates it.
		"continuation_state": store.ContinuationRetryArmed,
		"updated_at":         now,
	}
	// A zero wall is the caller saying this arming brought no news about where
	// the provider's window ends (a park that put the held-aside instant back,
	// a re-arm after a failed resume): the row keeps the wall it is on, and
	// with it the probe ledger of that wall. Writing the zero instant here
	// instead would make every such park look like a new wall — which is
	// exactly the max_attempts × budget ceiling this rule removes.
	if !wall.IsZero() {
		set[retryPath("wall")] = wall.UTC()
		// The probe BUDGET belongs to the WALL: it bounds the probes of one
		// provider window, so a charged park that merely re-states the
		// instant the row is already waiting for keeps it. Clearing it on
		// every charged park priced one window at max_attempts × budget —
		// 120 pods and 5 metered launches at the shipped defaults, against
		// 24 and 0 by design. The PACE (last_probe_at) is the row's and is
		// never written here: probeCountForWall says what clearing it cost.
		set[retryPath("probe_count")] = probeCountForWall(wall)
	}
	update := mongo.Pipeline{{{Key: "$set", Value: set}}}
	var updated struct {
		RetryState *store.RunRetryState `bson:"retry_state"`
	}
	err := s.runs.FindOneAndUpdate(ctx, filter, versionRunUpdate(update),
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

// DelayRunRetry moves an existing intent behind a shared circuit without
// charging a new attempt. The exact retry_after CAS means a stale sweeper can
// neither overwrite a newer arm nor resurrect an operator-resumed run.
func (s *Store) DelayRunRetry(ctx context.Context, runID string, expectedAfter, delayedUntil time.Time) (bool, error) {
	now := time.Now().UTC()
	filter := withTenantFilter(ctx, bson.M{
		"_id":                    runID,
		"status":                 string(store.RunStatusFailedResumable),
		retryPath("retry_after"): expectedAfter.UTC(),
	})
	// Runs armed by versions predating ScheduledAt need a fixed anchor on the
	// first deferral. Use the CAS-protected old retry_after as the best durable
	// origin and preserve it on every later delay.
	update := mongo.Pipeline{{{Key: "$set", Value: bson.M{
		retryPath("retry_after"): delayedUntil.UTC(),
		retryPath("scheduled_at"): bson.M{"$ifNull": bson.A{
			"$" + retryPath("scheduled_at"), expectedAfter.UTC(),
		}},
		retryPath("claimed_at"): "$$REMOVE",
		"updated_at":            now,
	}}}}
	res, err := s.runs.UpdateOne(ctx, filter, versionRunUpdate(update))
	if err != nil {
		return false, fmt.Errorf("store/mongo: delay retry %s: %w", runID, err)
	}
	return res.MatchedCount > 0, nil
}

// PullRunRetry pulls an armed retry forward to probeAt so the runner's usage
// preflight — the authoritative reading of the provider's window, and the one
// gate that refuses BEFORE an attempt costs anything — answers now instead of
// at the announced instant. An operator who cleared their forfait by hand is
// otherwise unobserved until the date the provider named, days away.
//
// It spends no attempt (the probe is speculative; the budget is for work that
// gets past the preflight) and leaves scheduled_at alone, for DelayRunRetry's
// reason: the max_wait anchor is not a probe's to move. The announced instant
// goes to probe_from, which the arming that follows puts back, and whose
// ABSENCE the filter requires — a second pull over a live marker would
// overwrite the provider's instant with a probe's own.
func (s *Store) PullRunRetry(ctx context.Context, runID string, expectedAfter, probeAt time.Time) (bool, error) {
	now := time.Now().UTC()
	filter := withTenantFilter(ctx, bson.M{
		"_id":                    runID,
		"status":                 string(store.RunStatusFailedResumable),
		retryPath("retry_after"): expectedAfter.UTC(),
		retryPath("probe_from"):  bson.M{"$exists": false},
	})
	update := bson.M{
		"$set": bson.M{
			retryPath("retry_after"):   probeAt.UTC(),
			retryPath("probe_from"):    expectedAfter.UTC(),
			retryPath("last_probe_at"): now,
			"updated_at":               now,
		},
		"$inc": bson.M{retryPath("probe_count"): 1, "version": 1},
	}
	res, err := s.runs.UpdateOne(ctx, filter, versionRunUpdate(update))
	if err != nil {
		return false, fmt.Errorf("store/mongo: pull retry %s: %w", runID, err)
	}
	return res.MatchedCount > 0, nil
}

// RearmProbedRunRetry re-arms a probe-marked retry at `at` and consumes the
// marker, without touching attempts — neither the counter nor the budget
// filter. Both matter: a probe that charged an attempt would retire a run
// days before the wall it waits on falls (five hourly probes against a
// four-day reset spend the default budget in five hours), and a probe taken
// on the LAST attempt the budget allows would be refused outright, ending the
// run the probe exists to save.
//
// The budget follows the same rule as the charged arming, and `wall` says
// which case this is: the zero time keeps the wall the row is on (the
// stand-down, and every restore of the held-aside instant), so probe_count
// survives — it bounds the probes of THAT wall, and a budget a probe's own
// re-arming could reset would bound nothing. A probed refusal naming a
// different instant passes it and starts the budget over, because that is a
// different wall. The PACE survives every arming: it belongs to the row.
func (s *Store) RearmProbedRunRetry(ctx context.Context, runID string, at, wall time.Time) (bool, error) {
	now := time.Now().UTC()
	filter := withTenantFilter(ctx, bson.M{
		"_id":                   runID,
		"status":                string(store.RunStatusFailedResumable),
		retryPath("probe_from"): bson.M{"$exists": true},
	})
	set := bson.M{
		retryPath("retry_after"): at.UTC(),
		retryPath("probe_from"):  "$$REMOVE",
		retryPath("claimed_at"):  "$$REMOVE",
		// The row was armed before the pull and stays armed: an
		// outcome consumer deferring to retry_armed must not see a
		// window where nothing is coming.
		"continuation_state": store.ContinuationRetryArmed,
		"updated_at":         now,
	}
	if !wall.IsZero() {
		set[retryPath("wall")] = wall.UTC()
		set[retryPath("probe_count")] = probeCountForWall(wall)
	}
	res, err := s.runs.UpdateOne(ctx, filter, versionRunUpdate(mongo.Pipeline{{{Key: "$set", Value: set}}}))
	if err != nil {
		return false, fmt.Errorf("store/mongo: re-arm probed retry %s: %w", runID, err)
	}
	return res.MatchedCount > 0, nil
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
	res, err := s.runs.UpdateOne(ctx, filter, versionRunUpdate(update))
	if err != nil {
		return false, fmt.Errorf("store/mongo: claim retry %s: %w", runID, err)
	}
	return res.MatchedCount > 0, nil
}

// ClearRunRetry disarms a retry that has been acted on — the successful
// resume's counterpart to the lease taken by ClaimRunRetry. Without it a
// retry_after in the past would survive on the row and re-fire the moment
// the resumed run failed again for an unrelated reason.
//
// The probe ledger deliberately survives it. Every resume clears the retry,
// including the one a probe just pulled forward, and it runs BEFORE the pod
// reaches the usage preflight — so clearing probe_from here would delete the
// marker precisely in the case it exists for, and clearing probe_count would
// hand each probe a fresh budget and make the bound unbounded. The next PAID
// arming is what resets the ledger.
func (s *Store) ClearRunRetry(ctx context.Context, runID string) error {
	update := bson.M{
		"$unset": bson.M{retryPath("retry_after"): "", retryPath("claimed_at"): ""},
		"$set":   bson.M{"updated_at": time.Now().UTC()},
		"$inc":   bson.M{"version": 1},
	}
	if _, err := s.runs.UpdateOne(ctx, withTenantFilter(ctx, bson.M{"_id": runID}), versionRunUpdate(update)); err != nil {
		return fmt.Errorf("store/mongo: clear retry %s: %w", runID, err)
	}
	return nil
}

// AbandonRunRetry records why a run stopped retrying and leaves it disarmed.
// Not conditioned on status: by the time we abandon, the reason is already
// permanent, and the note matters more than that race.
//
// It IS conditioned on the armed instant the caller read, when it has one.
// Settling is irreversible — the continuation goes final, so no pass lists
// the row again, and the marker holding the provider's own dated word is
// dropped — while the writes it races are one sweep tick away: a pull that
// moved the row forward to probe it, or a re-arm that put the announced
// instant back. Overwriting either retires a run days before the wall it is
// waiting on falls. A lost CAS reports abandoned=false, which is the honest
// answer: the row belongs to whoever moved it.
func (s *Store) AbandonRunRetry(ctx context.Context, runID, reason string, expectedAfter time.Time) (bool, error) {
	now := time.Now().UTC()
	filter := bson.M{"_id": runID}
	if !expectedAfter.IsZero() {
		filter[retryPath("retry_after")] = expectedAfter.UTC()
	}
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
		"$unset": bson.M{
			retryPath("retry_after"): "",
			// Nothing is armed, so there is no wait left to probe: a marker
			// surviving here would make the next arming of a
			// hand-resumed run free.
			retryPath("probe_from"): "",
		},
		"$inc": bson.M{"version": 1},
	}
	res, err := s.runs.UpdateOne(ctx, withTenantFilter(ctx, filter), versionRunUpdate(update))
	if err != nil {
		return false, fmt.Errorf("store/mongo: abandon retry %s: %w", runID, err)
	}
	return res.MatchedCount > 0, nil
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

// ListRunsForRetryProbe returns usage-window retries that are NOT yet due and
// are worth probing, soonest-armed first — the rows ListRunsDueForRetry
// structurally cannot see, which is why a reset an operator performs by hand
// goes unobserved until the provider's own date.
//
// floor does two jobs with one number: a row coming due within it is left to
// the due pass (probing it buys nothing), and a row probed within it is
// skipped, so the pace is one probe per floor per row whatever the sweeper's
// cadence. maxProbes bounds the probes of one wait; a non-positive budget
// means probing is off and the scan does not run — the filter alone would
// then still match every never-probed row.
func (s *Store) ListRunsForRetryProbe(ctx context.Context, now time.Time, floor time.Duration, maxProbes, limit int) ([]RetryDueRef, error) {
	if maxProbes <= 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	at := now.UTC()
	cur, err := s.runs.Find(ctx,
		withTenantFilter(ctx, bson.M{
			"status":                 string(store.RunStatusFailedResumable),
			retryPath("reason"):      store.RetryReasonUsageWindow,
			retryPath("retry_after"): bson.M{"$gt": at.Add(floor)},
			// A row already pulled forward is being settled by the due
			// pass; re-pulling it would overwrite the announced instant.
			retryPath("probe_from"): bson.M{"$exists": false},
			// The three bounds need $and: they are all $or, and one bson.M
			// cannot carry the key twice. Each needs its "field absent"
			// branch, for the type-bracketing reason ScheduleRunRetry
			// documents — {$lt: n} does not match a missing field, so
			// without it the FIRST probe of every run would be refused.
			"$and": []bson.M{
				{"$or": []bson.M{
					{retryPath("probe_count"): bson.M{"$exists": false}},
					{retryPath("probe_count"): bson.M{"$lt": maxProbes}},
				}},
				{"$or": []bson.M{
					{retryPath("last_probe_at"): bson.M{"$exists": false}},
					{retryPath("last_probe_at"): bson.M{"$lte": at.Add(-floor)}},
				}},
				// A live lease means a replica is acting on this row.
				{"$or": []bson.M{
					{retryPath("claimed_at"): bson.M{"$exists": false}},
					{retryPath("claimed_at"): bson.M{"$lt": at.Add(-retryClaimLease)}},
				}},
			},
		}),
		options.Find().
			SetProjection(bson.M{"_id": 1, "tenant_id": 1, "owner_id": 1, "bot_id": 1, "bot_source_tenant": 1, "file_path": 1, retryStateField: 1}).
			SetSort(bson.M{retryPath("retry_after"): 1}).
			SetLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list runs for retry probe: %w", err)
	}
	defer cur.Close(ctx)
	var out []RetryDueRef
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("store/mongo: decode runs for retry probe: %w", err)
	}
	return out, nil
}

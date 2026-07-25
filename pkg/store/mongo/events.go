package mongo

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// appendEventMaxRetries caps the seq-collision retry loop. Ten is
// generous: in production the engine is single-writer per run so a
// real collision only happens during a server-runner handoff. The
// generous budget exists for the storetest concurrency harness (8
// goroutines × 25 events) which deliberately drives the allocSeq
// path hard; flaking under that load would mask real regressions
// in the seq monotonicity contract.
const appendEventMaxRetries = 10

// appendEventBackoffBase is the base delay before retrying after a
// duplicate-key error. The actual delay grows linearly with the attempt
// number (0 ms on first attempt, base on the second, 2×base on the
// third) and carries ±25% jitter to spread retries when two processes
// genuinely race. Kept small because the underlying conflict is rare
// and resolves once one writer wins the next allocSeq round.
const appendEventBackoffBase = 20 * time.Millisecond

// AppendEvent allocates a monotonic per-run sequence via run_seq,
// then inserts the event document. The (run_id, seq) UNIQUE index on
// `events` is the safety net against two processes racing on seq
// allocation; on conflict we retry up to appendEventMaxRetries times,
// reallocating seq each iteration with a jittered backoff so a
// transient race does not surface as a hard run failure.
//
// Plan §D.3.
func (s *Store) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	// Tombstone guard: the events collection has no run-doc filter to
	// piggyback the predicate on, so a late writer's insert would
	// otherwise re-grow a deleted run's event stream.
	if err := s.guardNotDeleted(ctx, runID); err != nil {
		return nil, err
	}
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	evt.RunID = runID
	stampTenantOnEvent(ctx, &evt)
	// Stamp the run's current log byte position when the runner wired a
	// callback (SetLogPositionFn) — the cloud twin of the filesystem
	// store's stamping, powering the studio's per-node log slicing.
	// Only when the caller didn't pre-fill LogOffset.
	if evt.LogOffset == 0 {
		evt.LogOffset = s.logPositionFor(runID)
	}
	// Stamp the run's monotonic active duration (engine SharedBudget
	// elapsed) — the cloud twin of the filesystem store's ActiveMs
	// stamping, powering the studio's suspend-excluded active timer.
	if evt.ActiveMs == 0 {
		evt.ActiveMs = s.activeDurationFor(runID)
	}

	var lastSeq int64
	var lastErr error
	for attempt := 0; attempt < appendEventMaxRetries; attempt++ {
		if attempt > 0 {
			if err := backoffOrCancel(ctx, attempt); err != nil {
				return nil, err
			}
		}

		seq, err := s.allocSeq(ctx, runID)
		if err != nil {
			return nil, err
		}
		evt.Seq = seq
		lastSeq = seq

		if _, err := s.events.InsertOne(ctx, evt); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				// Another writer beat us at this seq; reallocate after a
				// brief jittered pause to let the winning writer commit.
				lastErr = err
				continue
			}
			return nil, fmt.Errorf("store/mongo: insert event %s/%d: %w", runID, seq, err)
		}
		return &evt, nil
	}
	return nil, fmt.Errorf("store/mongo: race on seq for run %s seq %d after %d attempts: %w", runID, lastSeq, appendEventMaxRetries, lastErr)
}

// backoffOrCancel sleeps for the attempt's jittered backoff window,
// returning the context error if the deadline fires first. attempt is
// 1-based on entry (callers skip the first attempt's pre-call so the
// happy path has zero delay).
func backoffOrCancel(ctx context.Context, attempt int) error {
	base := time.Duration(attempt) * appendEventBackoffBase
	jitter := time.Duration(rand.Int63n(int64(appendEventBackoffBase / 2)))
	delay := base - appendEventBackoffBase/4 + jitter

	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// allocSeq is the FindOneAndUpdate $inc pattern from plan §D.3, over the
// events counter (`next_seq`). See allocSeqField for the compound-key and
// backfill semantics shared with allocPlanSeq.
func (s *Store) allocSeq(ctx context.Context, runID string) (int64, error) {
	return s.allocSeqField(ctx, runID, "next_seq")
}

// allocPlanSeq is the plan-snapshot twin of allocSeq: a per-run monotonic
// counter for run_plans, keyed on the SAME {tenant_id, run_id} counter
// document but a distinct `next_plan_seq` field, so plan seqs advance
// independently of event seqs (both start at 0 per run). Replaces the
// former max-read+retry allocation, giving plans the same clean
// counter-backed seq-0 handling events already had.
// planSeqField is the run_seq counter field backing plan-snapshot seqs.
const planSeqField = "next_plan_seq"

func (s *Store) allocPlanSeq(ctx context.Context, runID string) (int64, error) {
	return s.allocSeqField(ctx, runID, planSeqField)
}

// allocSeqField is the shared FindOneAndUpdate $inc counter behind
// allocSeq / allocPlanSeq. The pre-image's field value is the seq we'll
// use; the post-image's is what the next caller sees. field lets one
// counter document hold several independent monotonic sequences.
//
// The _id is a {tenant_id, run_id} compound. Keying on run_id alone
// would let two tenants that happened to mint the same run_id (NewRunID
// uses time + crockford32 + 6 random chars — collision rare but not
// impossible across millions of runs) share a seq counter and stamp
// duplicate seq values on each other's documents.
//
// Backfill: existing documents with the plain-string _id keep working
// through the ErrNoDocuments path — the first alloc on the compound key
// creates a fresh counter starting at 0, which is correct as long as the
// legacy run is no longer writing. A counter document created by the
// sibling sequence (e.g. next_seq present, next_plan_seq absent) also
// starts the new field at 0 via the absent-field path below. Runs whose
// docs predate their counter self-heal at the write site: a duplicate-key
// collision re-seeds the counter from the persisted tail (seedSeqField).
// seedSeqField raises the run's counter field to at least floor ($max —
// monotonic, a concurrent $inc past floor is never rewound). Used to
// re-sync a counter that is BEHIND the persisted tail (docs written by
// the pre-counter max-read allocation).
func (s *Store) seedSeqField(ctx context.Context, runID, field string, floor int64) error {
	tenantID, _ := store.TenantFromContext(ctx)
	_, err := s.runSeq.UpdateOne(
		ctx,
		bson.M{"_id": bson.M{"tenant_id": tenantID, "run_id": runID}},
		bson.M{"$max": bson.M{field: floor}},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("store/mongo: seed seq %s for run %s: %w", field, runID, err)
	}
	return nil
}

func (s *Store) allocSeqField(ctx context.Context, runID, field string) (int64, error) {
	tenantID, _ := store.TenantFromContext(ctx)
	res := s.runSeq.FindOneAndUpdate(
		ctx,
		bson.M{"_id": bson.M{"tenant_id": tenantID, "run_id": runID}},
		bson.M{"$inc": bson.M{field: 1}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.Before),
	)
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			// Pre-image absent: the upsert just created the document
			// with field=1 (the $inc applied to the implicit zero).
			// Our seq is therefore 0 (the value before the increment).
			return 0, nil
		}
		return 0, fmt.Errorf("store/mongo: alloc seq %s: %w", runID, err)
	}
	raw, err := res.Raw()
	if err != nil {
		return 0, fmt.Errorf("store/mongo: decode seq %s: %w", runID, err)
	}
	// Field may be absent when the counter document was created by a
	// sibling sequence (e.g. events' next_seq exists but this is the
	// first plan snapshot for the run): absent → 0.
	if v, ok := raw.Lookup(field).AsInt64OK(); ok {
		return v, nil
	}
	return 0, nil
}

// LoadEvents returns every event for the run in seq-ascending order.
// Bounded by the BSON document size — for very long-running runs the
// caller should prefer LoadEventsRange or ScanEvents.
func (s *Store) LoadEvents(ctx context.Context, runID string) ([]*store.Event, error) {
	cur, err := s.events.Find(
		ctx,
		withTenantFilter(ctx, bson.M{"run_id": runID}),
		options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}),
	)
	if err != nil {
		return nil, fmt.Errorf("store/mongo: load events %s: %w", runID, err)
	}
	defer cur.Close(ctx)

	out := []*store.Event{}
	for cur.Next(ctx) {
		var e store.Event
		if err := cur.Decode(&e); err != nil {
			return nil, fmt.Errorf("store/mongo: decode event %s: %w", runID, err)
		}
		out = append(out, &e)
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("store/mongo: events cursor %s: %w", runID, err)
	}
	return out, nil
}

// LoadEventsRange returns events with from <= seq < to (or no upper
// bound when to == 0), capped at limit (or unbounded when limit == 0).
// Mirrors the FilesystemRunStore semantics for paginated reads.
func (s *Store) LoadEventsRange(ctx context.Context, runID string, from, to int64, limit int) ([]*store.Event, error) {
	filter := withTenantFilter(ctx, bson.M{
		"run_id": runID,
		"seq":    bson.M{"$gte": from},
	})
	if to > 0 {
		filter["seq"].(bson.M)["$lt"] = to
	}
	opts := options.Find().SetSort(bson.D{{Key: "seq", Value: 1}})
	if limit > 0 {
		opts.SetLimit(int64(limit))
	}
	cur, err := s.events.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("store/mongo: events range %s: %w", runID, err)
	}
	defer cur.Close(ctx)

	out := []*store.Event{}
	for cur.Next(ctx) {
		var e store.Event
		if err := cur.Decode(&e); err != nil {
			return nil, fmt.Errorf("store/mongo: decode event range %s: %w", runID, err)
		}
		out = append(out, &e)
	}
	return out, cur.Err()
}

// ScanEvents iterates events in seq order, calling visit until it
// returns false. Used by long-tail folds (snapshot reducer, runview
// list filter) to avoid materializing the full event slice.
func (s *Store) ScanEvents(ctx context.Context, runID string, visit func(*store.Event) bool) error {
	cur, err := s.events.Find(
		ctx,
		withTenantFilter(ctx, bson.M{"run_id": runID}),
		options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}),
	)
	if err != nil {
		return fmt.Errorf("store/mongo: scan events %s: %w", runID, err)
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var e store.Event
		if err := cur.Decode(&e); err != nil {
			return fmt.Errorf("store/mongo: decode scan event %s: %w", runID, err)
		}
		if !visit(&e) {
			return nil
		}
	}
	return cur.Err()
}

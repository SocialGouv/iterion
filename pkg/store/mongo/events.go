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

// appendEventMaxRetries caps the seq-collision retry loop. Three is
// generous: the engine is single-writer per run so a real collision
// only happens during a server-runner handoff. Anything beyond three
// is more likely a misconfiguration than transient contention.
const appendEventMaxRetries = 3

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
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	evt.RunID = runID
	stampTenantOnEvent(ctx, &evt)

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

// allocSeq is the FindOneAndUpdate $inc pattern from plan §D.3. The
// pre-image's next_seq is the seq we'll use; the post-image's
// next_seq is what the next caller will see.
//
// The _id is a {tenant_id, run_id} compound. Keying on run_id alone
// would let two tenants that happened to mint the same run_id (NewRunID
// uses time + crockford32 + 6 random chars — collision rare but not
// impossible across millions of runs) share a seq counter and stamp
// duplicate seq values on each other's events.
//
// Backfill: existing documents with the plain-string _id keep working
// through the ErrNoDocuments path — the first allocSeq on the compound
// key creates a fresh counter starting at 0, which is correct as long
// as the legacy run is no longer emitting events. For runs that are
// still actively writing, the migration tool needs to copy the
// next_seq value across; see scripts/migrate/run-seq-tenant-backfill.go.
func (s *Store) allocSeq(ctx context.Context, runID string) (int64, error) {
	tenantID, _ := store.TenantFromContext(ctx)
	res := s.runSeq.FindOneAndUpdate(
		ctx,
		bson.M{"_id": bson.M{"tenant_id": tenantID, "run_id": runID}},
		bson.M{"$inc": bson.M{"next_seq": 1}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.Before),
	)
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			// Pre-image absent: the upsert just created the document
			// with next_seq=1 (the $inc applied to the implicit zero).
			// Our seq is therefore 0 (the value before the increment).
			return 0, nil
		}
		return 0, fmt.Errorf("store/mongo: alloc seq %s: %w", runID, err)
	}
	var doc struct {
		NextSeq int64 `bson:"next_seq"`
	}
	if err := res.Decode(&doc); err != nil {
		return 0, fmt.Errorf("store/mongo: decode seq %s: %w", runID, err)
	}
	return doc.NextSeq, nil
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

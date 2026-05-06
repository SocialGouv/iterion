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

// AppendEvent allocates a monotonic per-run sequence via run_seq,
// then inserts the event document. The (run_id, seq) UNIQUE index on
// `events` is the safety net against two processes racing on seq
// allocation; on conflict the caller can retry.
//
// Plan §D.3.
func (s *Store) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	seq, err := s.allocSeq(ctx, runID)
	if err != nil {
		return nil, err
	}

	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	evt.RunID = runID
	evt.Seq = seq

	if _, err := s.events.InsertOne(ctx, evt); err != nil {
		// Duplicate seq → another process beat us; surface the error
		// so the caller can re-allocate. The current engine is
		// single-writer per run so this should be vanishingly rare.
		if mongo.IsDuplicateKeyError(err) {
			return nil, fmt.Errorf("store/mongo: race on seq for run %s seq %d: %w", runID, seq, err)
		}
		return nil, fmt.Errorf("store/mongo: insert event %s/%d: %w", runID, seq, err)
	}
	return &evt, nil
}

// allocSeq is the FindOneAndUpdate $inc pattern from plan §D.3. The
// pre-image's next_seq is the seq we'll use; the post-image's
// next_seq is what the next caller will see.
func (s *Store) allocSeq(ctx context.Context, runID string) (int64, error) {
	res := s.runSeq.FindOneAndUpdate(
		ctx,
		bson.M{"_id": runID},
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
		bson.M{"run_id": runID},
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
	filter := bson.M{
		"run_id": runID,
		"seq":    bson.M{"$gte": from},
	}
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
		bson.M{"run_id": runID},
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

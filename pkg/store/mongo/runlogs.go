package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/store"
)

// runLogDoc is one append-only chunk of a run's raw log byte stream
// (ADR-053). The runner pod is the single writer per run (per-pod
// sequential loop + run lock) and allocates offsets from a local
// counter seeded from RunLogSize at claim time; the unique
// (run_id, offset) index is the race/redelivery safety net. The server
// pod's log stream source tails inserts via a change stream and the
// REST/WS replay reads offset windows.
type runLogDoc struct {
	TenantID string    `bson:"tenant_id,omitempty"`
	RunID    string    `bson:"run_id"`
	Offset   int64     `bson:"offset"`
	Data     []byte    `bson:"data"`
	TS       time.Time `bson:"ts"`
}

// AppendRunLog implements store.RunLogStore: insert one chunk at the
// given offset. A duplicate (run_id, offset) means the same chunk was
// already persisted (runner redelivery / retry after a timed-out ack)
// and is skipped as an idempotent success.
func (s *Store) AppendRunLog(ctx context.Context, runID string, offset int64, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if offset < 0 {
		return fmt.Errorf("store/mongo: AppendRunLog(%s): negative offset %d", runID, offset)
	}
	doc := runLogDoc{
		RunID:  runID,
		Offset: offset,
		Data:   data,
		TS:     time.Now().UTC(),
	}
	if id, ok := store.TenantFromContext(ctx); ok {
		doc.TenantID = id
	}
	if _, err := s.runLogs.InsertOne(ctx, doc); err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return nil // idempotent redelivery of an already-persisted chunk
		}
		return fmt.Errorf("store/mongo: append run log %s@%d (%d bytes): %w", runID, offset, len(data), err)
	}
	return nil
}

// ReadRunLogRange implements store.RunLogStore: reassemble bytes
// [from, until) from the chunk documents overlapping the window;
// until <= 0 means "to the current end". Chunks are contiguous by
// construction (single writer, sequential offsets), so the concat of
// overlapping chunks sliced at the window edges is the exact range. A
// run with no persisted log yields (nil, nil).
func (s *Store) ReadRunLogRange(ctx context.Context, runID string, from, until int64) ([]byte, error) {
	if from < 0 {
		from = 0
	}
	if until > 0 && from >= until {
		return nil, nil
	}
	filter := withTenantFilter(ctx, bson.M{"run_id": runID})
	// Bound the scan below by the anchor chunk straddling `from`
	// (chunks are contiguous, so everything starting before it ends at
	// or before it) — a tail-poll with a growing `from` must not
	// re-fetch and decode the run's whole chunk history on every read.
	offsetRange := bson.M{}
	if from > 0 {
		anchorFilter := withTenantFilter(ctx, bson.M{"run_id": runID, "offset": bson.M{"$lte": from}})
		var anchor struct {
			Offset int64 `bson:"offset"`
		}
		if err := s.runLogs.FindOne(ctx, anchorFilter,
			options.FindOne().SetSort(bson.D{{Key: "offset", Value: -1}}).SetProjection(bson.M{"offset": 1}),
		).Decode(&anchor); err == nil {
			offsetRange["$gte"] = anchor.Offset
		}
	}
	if until > 0 {
		offsetRange["$lt"] = until
	}
	if len(offsetRange) > 0 {
		filter["offset"] = offsetRange
	}
	cur, err := s.runLogs.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "offset", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("store/mongo: run log range %s: %w", runID, err)
	}
	defer cur.Close(ctx)

	var out []byte
	if until > 0 {
		out = make([]byte, 0, until-from)
	}
	for cur.Next(ctx) {
		var doc runLogDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("store/mongo: decode run log chunk %s: %w", runID, err)
		}
		end := doc.Offset + int64(len(doc.Data))
		if end <= from {
			continue // the anchor chunk itself may end exactly at `from`
		}
		data := doc.Data
		off := doc.Offset
		if off < from {
			data = data[from-off:]
			off = from
		}
		if until > 0 && off+int64(len(data)) > until {
			data = data[:until-off]
		}
		out = append(out, data...)
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("store/mongo: run log cursor %s: %w", runID, err)
	}
	return out, nil
}

// RunLogSize implements store.RunLogStore: the end offset of the last
// persisted chunk (the offset the next append should use), 0 when the
// run has no log.
func (s *Store) RunLogSize(ctx context.Context, runID string) (int64, error) {
	res := s.runLogs.FindOne(
		ctx,
		withTenantFilter(ctx, bson.M{"run_id": runID}),
		options.FindOne().SetSort(bson.D{{Key: "offset", Value: -1}}),
	)
	var doc runLogDoc
	if err := res.Decode(&doc); err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil
		}
		return 0, fmt.Errorf("store/mongo: run log size %s: %w", runID, err)
	}
	return doc.Offset + int64(len(doc.Data)), nil
}

// RunLogsCollection exposes the run_logs collection so the runview
// Mongo log stream source can open change streams against the same
// database the store writes to. Same caveat as EventsCollection —
// layering shortcut, not the long-term API.
func (s *Store) RunLogsCollection() *mongo.Collection { return s.runLogs }

// SetLogPositionFn installs the callback AppendEvent uses to stamp
// Event.LogOffset — the cloud twin of FilesystemRunStore's hook. The
// runner wires it to its per-run log writer's running byte total so
// the studio's per-node log slicing works against run_logs too. Pass
// nil to disable stamping.
func (s *Store) SetLogPositionFn(fn store.LogPositionFn) {
	s.logPositionMu.Lock()
	s.logPositionFn = fn
	s.logPositionMu.Unlock()
}

// logPositionFor returns the running log byte total for runID, or 0
// when no callback is installed (server pods, tests).
func (s *Store) logPositionFor(runID string) int64 {
	s.logPositionMu.Lock()
	fn := s.logPositionFn
	s.logPositionMu.Unlock()
	if fn == nil {
		return 0
	}
	return fn(runID)
}

// SetActiveDurationFn installs the callback AppendEvent uses to stamp
// Event.ActiveMs — the cloud twin of FilesystemRunStore's hook. The
// runner wires it to its per-run engine's monotonic SharedBudget
// elapsed. Pass nil to disable stamping.
func (s *Store) SetActiveDurationFn(fn store.ActiveDurationFn) {
	s.logPositionMu.Lock()
	s.activeDurationFn = fn
	s.logPositionMu.Unlock()
}

// activeDurationFor returns the run's monotonic active duration in ms,
// or 0 when no callback is installed (server pods, tests) or the run
// isn't held by this process.
func (s *Store) activeDurationFor(runID string) int64 {
	s.logPositionMu.Lock()
	fn := s.activeDurationFn
	s.logPositionMu.Unlock()
	if fn == nil {
		return 0
	}
	return fn(runID)
}

package mongo

import (
	"context"
	"fmt"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

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
		Data:   slices.Clone(data),
		TS:     time.Now().UTC(),
	}
	if id, ok := store.TenantFromContext(ctx); ok {
		doc.TenantID = id
	}
	return s.withRunMetadataWrite(ctx, runID, func(sc context.Context, parent *store.Run) error {
		n, e := s.streamNamespace(sc, runID, parent)
		if e != nil {
			return e
		}
		row := bson.M{"tenant_id": doc.TenantID, "run_id": doc.RunID, "offset": doc.Offset, "data": doc.Data, "ts": doc.TS}
		n.stamp(row)
		res, err := n.logs.UpdateOne(sc,
			n.filter(sc, bson.M{"offset": offset}),
			bson.M{"$setOnInsert": row}, options.UpdateOne().SetUpsert(true))
		if err != nil {
			return fmt.Errorf("store/mongo: append run log %s@%d (%d bytes): %w", runID, offset, len(data), err)
		}
		if res.UpsertedCount == 0 {
			return errValuePublicationNoop
		}
		return nil
	})
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
	var out []byte
	err := s.withOpaqueStreamRead(ctx, runID, func(sc context.Context, n streamDataNamespace, _ *store.Run) error {
		out = nil
		if until > 0 && from >= until {
			return nil
		}
		var e error
		out, e = s.readStreamLogRange(sc, n, from, until)
		return e
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Store) readStreamLogRange(ctx context.Context, n streamDataNamespace, from, until int64) ([]byte, error) {
	filter := n.filter(ctx, nil)
	r := bson.M{}
	if from > 0 {
		var anchor struct {
			Offset int64 `bson:"offset"`
		}
		e := n.logs.FindOne(ctx, n.filter(ctx, bson.M{"offset": bson.M{"$lte": from}}), options.FindOne().SetSort(bson.D{{Key: "offset", Value: -1}}).SetProjection(bson.M{"offset": 1})).Decode(&anchor)
		if e == nil {
			r["$gte"] = anchor.Offset
		} else if e != mongo.ErrNoDocuments {
			return nil, e
		}
	}
	if until > 0 {
		r["$lt"] = until
	}
	if len(r) > 0 {
		filter["offset"] = r
	}
	cur, e := n.logs.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "offset", Value: 1}}))
	if e != nil {
		return nil, e
	}
	defer cur.Close(ctx)
	var out []byte
	for cur.Next(ctx) {
		var doc runLogDoc
		if e := cur.Decode(&doc); e != nil {
			return nil, e
		}
		end := doc.Offset + int64(len(doc.Data))
		if end <= from {
			continue
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
	if e = cur.Err(); e != nil {
		return nil, e
	}
	return out, nil
}
func (s *Store) RunLogSize(ctx context.Context, runID string) (int64, error) {
	var size int64
	err := s.withOpaqueStreamRead(ctx, runID, func(sc context.Context, n streamDataNamespace, _ *store.Run) error {
		size = 0
		var d runLogDoc
		e := n.logs.FindOne(sc, n.filter(sc, nil), options.FindOne().SetSort(bson.D{{Key: "offset", Value: -1}})).Decode(&d)
		if e == mongo.ErrNoDocuments {
			return nil
		}
		if e != nil {
			return e
		}
		size = d.Offset + int64(len(d.Data))
		return nil
	})
	if err != nil {
		return 0, err
	}
	return size, nil
}

// RunLogsCollection remains the historical direct collection for legacy adapters.
// Strict streaming requires the separate scoped adapter before activation.
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

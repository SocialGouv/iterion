package runstream

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// mongoTerminalPollInterval bounds how often a log OR event subscription
// re-reads the run document to detect a terminal status — the Mongo
// twin of FileSource's run.json poll (the run executes on a runner pod;
// the server has no in-process completion signal). Swappable for a
// runs-collection change stream later without contract change.
var mongoTerminalPollInterval = 5 * time.Second

// runLogChunkDoc mirrors the run_logs document shape written by
// pkg/store/mongo (runLogDoc) — the fields the stream reads.
type runLogChunkDoc struct {
	RunID  string `bson:"run_id"`
	Offset int64  `bson:"offset"`
	Data   []byte `bson:"data"`
}

// SubscribeLogs streams the run's persisted log chunks from fromOffset:
// backfill from the run_logs collection, then a change-stream tail —
// the same watch-first pipeline as SubscribeEvents, with byte offsets
// as the dedup anchor instead of seqs. The stream closes (after a final
// backfill) once the run document reaches a terminal status.
func (m *MongoSource) SubscribeLogs(ctx context.Context, runID string, fromOffset int64) (LogSubscription, error) {
	if m.runLogs == nil {
		return nil, ErrLogsUnsupported
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fromOffset < 0 {
		fromOffset = 0
	}
	subCtx, cancel := context.WithCancel(ctx)
	pipe := NewLogPipeWithClose(cancel)
	go func() {
		defer cancel()
		m.runLogStream(subCtx, runID, fromOffset, pipe)
	}()
	return pipe, nil
}

// runLogStream is the subscription pump: watch-first rounds driven by
// the shared reconnect loop, with terminal-status polling and a final
// backfill so every chunk persisted before the terminal flip is
// delivered before the channel closes.
func (m *MongoSource) runLogStream(ctx context.Context, runID string, fromOffset int64, pipe LogPipe) {
	defer pipe.Finish(nil)
	delivered := fromOffset
	reconnectLoop(ctx, func(delay time.Duration, err error) {
		pipe.Warn(fmt.Errorf("runstream/mongo: log stream (will reconnect in %s): %w", delay, err))
	}, func() (bool, error) {
		next, terminal, err := m.streamLogsOnce(ctx, runID, delivered, pipe)
		delivered = next
		return terminal, err // nil error without terminal = parent ctx ended
	})
}

// streamLogsOnce runs one watch-first attempt: open the change stream,
// backfill [from, end) from the collection, then pump inserts deduped
// by byte offset. A side poller watches the run's status and cancels
// the round on a terminal flip; the caller then gets terminal=true
// after this round's final drain. Returns the delivered high-water
// offset. A nil error with terminal=false means the parent ctx ended.
func (m *MongoSource) streamLogsOnce(ctx context.Context, runID string, from int64, pipe LogPipe) (delivered int64, terminal bool, err error) {
	delivered = from

	roundCtx, cancelRound := context.WithCancel(ctx)
	defer cancelRound()

	var isTerminal atomic.Bool
	go func() {
		t := time.NewTicker(mongoTerminalPollInterval)
		defer t.Stop()
		for {
			select {
			case <-roundCtx.Done():
				return
			case <-t.C:
				if m.runIsTerminal(roundCtx, runID) {
					isTerminal.Store(true)
					cancelRound()
					return
				}
			}
		}
	}()

	matchExpr := bson.M{
		"operationType":       "insert",
		"fullDocument.run_id": runID,
	}
	if tenantID, ok := store.TenantFromContext(ctx); ok {
		matchExpr["fullDocument.tenant_id"] = tenantID
	}
	pipeline := mongo.Pipeline{bson.D{{Key: "$match", Value: matchExpr}}}
	stream, werr := m.runLogs.Watch(roundCtx, pipeline, options.ChangeStream().SetFullDocument(options.UpdateLookup))
	if werr != nil {
		return delivered, isTerminal.Load(), fmt.Errorf("open change stream: %w", werr)
	}
	defer stream.Close(ctx)

	delivered, berr := m.backfillLogs(roundCtx, runID, delivered, pipe)
	if berr != nil && !errors.Is(berr, context.Canceled) {
		return delivered, isTerminal.Load(), fmt.Errorf("backfill: %w", berr)
	}

	for stream.Next(roundCtx) {
		var doc struct {
			FullDocument runLogChunkDoc `bson:"fullDocument"`
		}
		if derr := stream.Decode(&doc); derr != nil {
			pipe.Warn(fmt.Errorf("runstream/mongo: decode log change: %w", derr))
			continue
		}
		var ok bool
		if delivered, ok = ShipLogChunk(roundCtx, pipe, doc.FullDocument.Offset, doc.FullDocument.Data, delivered); !ok {
			return delivered, false, nil
		}
	}

	if isTerminal.Load() {
		// Final drain on the PARENT ctx (the round ctx is cancelled):
		// chunks persisted before the terminal flip must be delivered
		// before the close.
		if d, ferr := m.backfillLogs(ctx, runID, delivered, pipe); ferr == nil {
			delivered = d
		}
		return delivered, true, nil
	}
	if serr := stream.Err(); serr != nil && !errors.Is(serr, context.Canceled) {
		return delivered, false, serr
	}
	return delivered, false, nil
}

// backfillLogs ships persisted chunks whose end is past `from`, sliced
// at the boundary, in offset order. The query is bounded below by the
// anchor chunk straddling `from` (chunks are contiguous, so everything
// starting before the anchor ends at or before it) — without the bound,
// every reconnect round and terminal drain would re-fetch and decode
// the run's entire chunk history just to discard it client-side.
// Returns the new delivered offset.
func (m *MongoSource) backfillLogs(ctx context.Context, runID string, from int64, pipe LogPipe) (int64, error) {
	base := bson.M{"run_id": runID}
	if tenantID, ok := store.TenantFromContext(ctx); ok {
		base["tenant_id"] = tenantID
	}
	filter := bson.M{"run_id": base["run_id"]}
	for k, v := range base {
		filter[k] = v
	}
	if anchor, ok := logAnchorOffset(ctx, m.runLogs, base, from); ok {
		filter["offset"] = bson.M{"$gte": anchor}
	}
	cur, err := m.runLogs.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "offset", Value: 1}}))
	if err != nil {
		return from, err
	}
	defer cur.Close(ctx)

	delivered := from
	for cur.Next(ctx) {
		var doc runLogChunkDoc
		if err := cur.Decode(&doc); err != nil {
			return delivered, err
		}
		var ok bool
		if delivered, ok = ShipLogChunk(ctx, pipe, doc.Offset, doc.Data, delivered); !ok {
			return delivered, ctx.Err()
		}
	}
	return delivered, cur.Err()
}

// logAnchorOffset locates the start of the newest chunk at or before
// `from` via the (run_id, offset) index — the lower bound for an exact
// window read over contiguous chunks. ok=false when no such chunk
// exists (read from the beginning).
func logAnchorOffset(ctx context.Context, coll *mongo.Collection, base bson.M, from int64) (int64, bool) {
	if from <= 0 {
		return 0, false
	}
	anchorFilter := bson.M{"offset": bson.M{"$lte": from}}
	for k, v := range base {
		anchorFilter[k] = v
	}
	var doc struct {
		Offset int64 `bson:"offset"`
	}
	err := coll.FindOne(ctx, anchorFilter,
		options.FindOne().SetSort(bson.D{{Key: "offset", Value: -1}}).SetProjection(bson.M{"offset": 1}),
	).Decode(&doc)
	if err != nil {
		return 0, false
	}
	return doc.Offset, true
}

// runIsTerminal reads the run document's status. Errors (including
// not-found) read as "not terminal" — the poll just tries again.
func (m *MongoSource) runIsTerminal(ctx context.Context, runID string) bool {
	if m.runs == nil {
		return false
	}
	filter := bson.M{"_id": runID}
	if tenantID, ok := store.TenantFromContext(ctx); ok {
		filter["tenant_id"] = tenantID
	}
	var doc struct {
		Status store.RunStatus `bson:"status"`
	}
	opts := options.FindOne().SetProjection(bson.M{"status": 1})
	if err := m.runs.FindOne(ctx, filter, opts).Decode(&doc); err != nil {
		return false
	}
	return doc.Status.IsTerminal()
}

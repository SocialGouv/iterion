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

// mongoLogTerminalPollInterval bounds how often a log subscription
// re-reads the run document to detect a terminal status — the Mongo
// twin of FileSource's run.json poll (the run executes on a runner pod;
// the server has no in-process completion signal). Swappable for a
// runs-collection change stream later without contract change.
var mongoLogTerminalPollInterval = 5 * time.Second

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
	pipe := NewLogPipe()
	go func() {
		defer cancel()
		m.runLogStream(subCtx, runID, fromOffset, pipe)
	}()
	return logPipeCloser{LogPipe: pipe, cancel: cancel}, nil
}

// logPipeCloser propagates a consumer Close to the subscription ctx so
// the producer goroutine (blocked in stream.Next) unwinds promptly.
type logPipeCloser struct {
	LogPipe
	cancel context.CancelFunc
}

func (c logPipeCloser) Close() error {
	c.cancel()
	return c.LogPipe.Close()
}

// runLogStream is the subscription pump: watch-first rounds with
// exponential reconnect backoff, terminal-status polling, and a final
// backfill so every chunk persisted before the terminal flip is
// delivered before the channel closes.
func (m *MongoSource) runLogStream(ctx context.Context, runID string, fromOffset int64, pipe LogPipe) {
	defer pipe.Finish(nil)

	const baseBackoff = 250 * time.Millisecond
	const maxBackoff = 30 * time.Second
	backoff := baseBackoff
	delivered := fromOffset

	for {
		if ctx.Err() != nil {
			return
		}
		next, terminal, err := m.streamLogsOnce(ctx, runID, delivered, pipe)
		delivered = next
		if terminal {
			return
		}
		if err == nil {
			return // parent ctx cancelled cleanly
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		pipe.Warn(fmt.Errorf("runstream/mongo: log stream (will reconnect in %s): %w", backoff, err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
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
		t := time.NewTicker(mongoLogTerminalPollInterval)
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
		if delivered, ok = shipLogChunk(roundCtx, pipe, doc.FullDocument, delivered); !ok {
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
// at the boundary, in offset order. Returns the new delivered offset.
func (m *MongoSource) backfillLogs(ctx context.Context, runID string, from int64, pipe LogPipe) (int64, error) {
	filter := bson.M{"run_id": runID}
	if tenantID, ok := store.TenantFromContext(ctx); ok {
		filter["tenant_id"] = tenantID
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
		if delivered, ok = shipLogChunk(ctx, pipe, doc, delivered); !ok {
			return delivered, ctx.Err()
		}
	}
	return delivered, cur.Err()
}

// shipLogChunk delivers one chunk deduped/sliced against the delivered
// high-water offset. Returns the new offset and false when the
// subscription is over.
func shipLogChunk(ctx context.Context, pipe LogPipe, doc runLogChunkDoc, delivered int64) (int64, bool) {
	end := doc.Offset + int64(len(doc.Data))
	if end <= delivered {
		return delivered, true
	}
	data := doc.Data
	off := doc.Offset
	if off < delivered {
		data = data[delivered-off:]
		off = delivered
	}
	if !pipe.Ship(ctx, LogChunk{Offset: off, Data: data, Total: end}) {
		return delivered, false
	}
	return end, true
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

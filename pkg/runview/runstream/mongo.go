package runstream

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// MongoSource implements Source on top of MongoDB change streams + a
// backfill range query, for runs living in the cloud (Mongo) store.
//
// Each SubscribeEvents runs an open-watch-first pipeline, per attempt:
//
//  1. Open the change stream filtered to inserts on the run with
//     seq >= fromSeq. Nothing is consumed yet, but from this moment the
//     stream is recording — an insert can no longer fall into a blind
//     window.
//  2. Backfill: read persisted events with seq >= fromSeq until the
//     cursor drains, shipping batches in order; record maxSeq.
//  3. Tail: pump the change stream, dropping seq <= maxSeq (the overlap
//     with the backfill), shipping each insert as a 1-event batch.
//
// The pre-fix ordering (replay, THEN open the stream) lost any event
// inserted between the two phases — a change stream never sees the
// past. Watch-first + seq dedup closes that gap, and the same pattern
// makes reconnects lossless too: every retry re-opens the stream and
// re-backfills from the last delivered seq.
//
// The whole pipeline runs on the subscription's goroutine — Subscribe
// never blocks on a slow consumer (the previous implementation replayed
// synchronously into the buffered channel and could deadlock on a
// backlog larger than the buffer, since the caller only starts reading
// after Subscribe returns).
//
// MongoDB requires a replica set for change streams; the
// docker-compose.cloud.yml stack initiates `rs0` automatically.
type MongoSource struct {
	events  *mongo.Collection
	runLogs *mongo.Collection // nil disables log streaming (Capabilities.Logs)
	runs    *mongo.Collection // terminal-status polling for the log stream
	logger  *iterlog.Logger
	metrics *metrics.Registry
}

// NewMongo builds a Source backed by the store's collections (the
// Mongo RunStore's EventsCollection / RunLogsCollection /
// RunsCollection accessors). runLogs and runs may be nil to disable
// log streaming (Capabilities().Logs == false).
func NewMongo(events, runLogs, runs *mongo.Collection, logger *iterlog.Logger) *MongoSource {
	if logger == nil {
		logger = iterlog.New(iterlog.LevelInfo, nil)
	}
	return &MongoSource{events: events, runLogs: runLogs, runs: runs, logger: logger}
}

// WithMetrics attaches a Prometheus registry so each delivered event
// updates iterion_mongo_change_stream_lag_seconds. Optional — passing
// no metrics keeps the source silent.
func (m *MongoSource) WithMetrics(reg *metrics.Registry) *MongoSource {
	m.metrics = reg
	return m
}

// Capabilities advertises live tail + historical range for both
// streams; Logs reflects whether the run_logs collection was wired.
func (m *MongoSource) Capabilities() Capabilities {
	return Capabilities{LiveTail: true, HistoricalRange: true, Logs: m.runLogs != nil}
}

// Close is a no-op — the source itself owns no long-lived resources.
// Subscriptions own their cursors + change streams.
func (m *MongoSource) Close() error { return nil }

// SubscribeEvents spawns the watch-first pipeline described on
// MongoSource. The returned subscription is usable immediately;
// backfill batches arrive first, then live inserts.
func (m *MongoSource) SubscribeEvents(ctx context.Context, runID string, fromSeq int64) (EventSubscription, error) {
	// Honour an already-cancelled ctx so callers don't get a
	// subscription whose Events() channel closes immediately — the WS
	// handler reads "channel closed" as "stream ended cleanly".
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	subCtx, cancel := context.WithCancel(ctx)
	sub := &mongoEventSub{
		events: make(chan []*store.Event, 8),
		errors: make(chan error, 4),
		cancel: cancel,
	}
	go m.runEvents(subCtx, runID, fromSeq, sub)
	return sub, nil
}

// runEvents is the subscription pump: streamEventsOnce with exponential
// reconnect backoff (250ms → 30s ceiling). fromSeq advances past every
// delivered event so a reconnect neither loses nor re-delivers.
func (m *MongoSource) runEvents(ctx context.Context, runID string, fromSeq int64, sub *mongoEventSub) {
	defer close(sub.events)
	defer close(sub.errors)

	const baseBackoff = 250 * time.Millisecond
	const maxBackoff = 30 * time.Second
	backoff := baseBackoff

	for {
		if ctx.Err() != nil {
			return
		}
		nextSeq, err := m.streamEventsOnce(ctx, runID, fromSeq, sub)
		if err == nil {
			// Clean exit — ctx cancelled.
			return
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		// Surface the error (non-fatal) and reconnect.
		select {
		case sub.errors <- fmt.Errorf("runstream/mongo: events stream (will reconnect in %s): %w", backoff, err):
		default:
			m.logger.Warn("runstream/mongo: errors channel full; dropping reconnect notice")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		fromSeq = nextSeq
	}
}

// streamEventsOnce runs one open-watch-first attempt: change stream
// (seq >= fromSeq) opened before the backfill Find, then the stream is
// pumped with a seq <= maxBackfilled dedup. Returns the seq the next
// attempt should resume from (last delivered + 1). A nil error means
// ctx was cancelled cleanly; non-nil means backoff + reconnect.
func (m *MongoSource) streamEventsOnce(ctx context.Context, runID string, fromSeq int64, sub *mongoEventSub) (int64, error) {
	matchExpr := bson.M{
		"operationType":       "insert",
		"fullDocument.run_id": runID,
		"fullDocument.seq":    bson.M{"$gte": fromSeq},
	}
	// Scope the stream to the caller's tenant when ctx carries one
	// (server WS handlers stamp tenant via auth middleware). When the
	// caller is privileged (cluster-admin diagnostics), pass through
	// unscoped.
	if tenantID, ok := store.TenantFromContext(ctx); ok {
		matchExpr["fullDocument.tenant_id"] = tenantID
	}
	pipeline := mongo.Pipeline{bson.D{{Key: "$match", Value: matchExpr}}}
	stream, err := m.events.Watch(ctx, pipeline, options.ChangeStream().SetFullDocument(options.UpdateLookup))
	if err != nil {
		return fromSeq, fmt.Errorf("open change stream: %w", err)
	}
	defer stream.Close(ctx)

	maxSeq, err := m.backfillEvents(ctx, runID, fromSeq, sub)
	if err != nil {
		// Whatever was shipped is accounted for in maxSeq; resume past it.
		return maxSeq + 1, fmt.Errorf("backfill: %w", err)
	}

	for stream.Next(ctx) {
		var doc struct {
			FullDocument store.Event `bson:"fullDocument"`
		}
		if err := stream.Decode(&doc); err != nil {
			// A single bad doc is not fatal — log and continue rather
			// than tearing down the whole stream.
			select {
			case sub.errors <- fmt.Errorf("runstream/mongo: decode change: %w", err):
			default:
			}
			continue
		}
		if doc.FullDocument.Seq <= maxSeq {
			continue // overlap with the backfill window
		}
		evt := doc.FullDocument
		select {
		case sub.events <- []*store.Event{&evt}:
		case <-ctx.Done():
			return maxSeq + 1, nil
		}
		maxSeq = evt.Seq
		// Lag is wall-clock latency from event creation (set by the
		// store at AppendEvent time) to delivery on the change-stream
		// pipeline. Operators alert on sustained lag > a few seconds.
		if m.metrics != nil && !evt.Timestamp.IsZero() {
			m.metrics.MongoChangeStreamLagS.Set(time.Since(evt.Timestamp).Seconds())
		}
	}
	if err := stream.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return maxSeq + 1, err
	}
	// Stream exited cleanly — usually means ctx was cancelled.
	return maxSeq + 1, nil
}

// backfillEvents drains the events collection for runID, seq >= fromSeq,
// into the subscription in batches of up to MaxEventsPerPage. Returns
// the highest seq shipped (fromSeq-1 when nothing matched).
func (m *MongoSource) backfillEvents(ctx context.Context, runID string, fromSeq int64, sub *mongoEventSub) (int64, error) {
	filter := bson.M{
		"run_id": runID,
		"seq":    bson.M{"$gte": fromSeq},
	}
	if tenantID, ok := store.TenantFromContext(ctx); ok {
		filter["tenant_id"] = tenantID
	}
	cur, err := m.events.Find(
		ctx,
		filter,
		options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}),
	)
	if err != nil {
		return fromSeq - 1, err
	}
	defer cur.Close(ctx)

	maxSeq := fromSeq - 1
	batch := make([]*store.Event, 0, 256)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		select {
		case sub.events <- batch:
			batch = make([]*store.Event, 0, 256)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for cur.Next(ctx) {
		var e store.Event
		if err := cur.Decode(&e); err != nil {
			return maxSeq, err
		}
		batch = append(batch, &e)
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
		if len(batch) >= MaxEventsPerPage {
			if err := flush(); err != nil {
				return maxSeq, err
			}
		}
	}
	if err := cur.Err(); err != nil {
		return maxSeq, err
	}
	if err := flush(); err != nil {
		return maxSeq, err
	}
	return maxSeq, nil
}

type mongoEventSub struct {
	events chan []*store.Event
	errors chan error
	cancel context.CancelFunc
	once   sync.Once
}

func (s *mongoEventSub) Events() <-chan []*store.Event { return s.events }
func (s *mongoEventSub) Errors() <-chan error          { return s.errors }
func (s *mongoEventSub) Close() error {
	s.once.Do(s.cancel)
	return nil
}

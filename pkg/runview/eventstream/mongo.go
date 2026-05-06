package eventstream

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// MongoSource implements Source on top of MongoDB change streams +
// a backfill range query against the events collection. Two phases
// per Subscribe:
//
//  1. Replay: read events with seq >= fromSeq until the cursor is
//     drained. Push each into the Events channel in order.
//  2. Tail:   open a change-stream filtered to inserts on the same
//     run whose seq is greater than the highest seq seen in phase 1.
//     Push every change into the same channel.
//
// The two phases are joined cleanly so a client never observes a
// gap (we record the max seq from the replay before opening the
// change stream so concurrent inserts during the gap window are
// captured by the tail phase).
//
// MongoDB requires a replica set for change streams; the
// docker-compose.cloud.yml stack initiates `rs0` automatically.
type MongoSource struct {
	events *mongo.Collection
	logger *iterlog.Logger
}

// NewMongo builds a Source backed by the supplied events collection.
// The collection must be the same one MongoRunStore writes to —
// pkg/store/mongo.Store.RunsCollection has a sibling exposing it.
func NewMongo(events *mongo.Collection, logger *iterlog.Logger) *MongoSource {
	if logger == nil {
		logger = iterlog.New(iterlog.LevelInfo, nil)
	}
	return &MongoSource{events: events, logger: logger}
}

// Capabilities advertises live tail + historical range backed by the
// change stream + the events collection respectively.
func (m *MongoSource) Capabilities() SourceCapabilities {
	return SourceCapabilities{LiveTail: true, HistoricalRange: true}
}

// Close is a no-op — the source itself owns no long-lived resources.
// Subscriptions own their cursors + change streams.
func (m *MongoSource) Close() error { return nil }

// Subscribe runs the replay phase synchronously, then spawns a
// goroutine for the change-stream tail. The returned Subscription is
// usable as soon as Subscribe returns (Events() will receive backfill
// items first).
func (m *MongoSource) Subscribe(ctx context.Context, runID string, fromSeq int64) (Subscription, error) {
	subCtx, cancel := context.WithCancel(ctx)

	sub := &mongoSubscription{
		events: make(chan *store.Event, 64),
		errors: make(chan error, 4),
		cancel: cancel,
	}

	maxSeq, err := m.replay(subCtx, runID, fromSeq, sub.events)
	if err != nil {
		cancel()
		close(sub.events)
		close(sub.errors)
		return nil, fmt.Errorf("eventstream/mongo: replay: %w", err)
	}

	// Tail phase. The pipeline filters to the run + seq strictly
	// greater than what we backfilled so we don't double-deliver
	// the boundary event.
	go m.tail(subCtx, runID, maxSeq, sub)

	return sub, nil
}

// replay drains the events collection for runID, seq >= fromSeq, into
// out. Returns the highest seq observed (so the change-stream phase
// can filter strictly above it).
func (m *MongoSource) replay(ctx context.Context, runID string, fromSeq int64, out chan<- *store.Event) (int64, error) {
	cur, err := m.events.Find(
		ctx,
		bson.M{
			"run_id": runID,
			"seq":    bson.M{"$gte": fromSeq},
		},
		options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}),
	)
	if err != nil {
		return 0, err
	}
	defer cur.Close(ctx)

	maxSeq := fromSeq - 1
	for cur.Next(ctx) {
		var e store.Event
		if err := cur.Decode(&e); err != nil {
			return maxSeq, err
		}
		select {
		case out <- &e:
		case <-ctx.Done():
			return maxSeq, ctx.Err()
		}
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	if err := cur.Err(); err != nil {
		return maxSeq, err
	}
	return maxSeq, nil
}

// tail runs the change-stream phase. Closes sub.events + sub.errors
// when ctx is cancelled. On a transient stream error we surface it
// (so the WS layer can react if it cares) AND reconnect with
// exponential backoff up to a 30s ceiling. fromSeq is bumped after
// each event so a reconnect doesn't replay events the consumer
// already saw.
func (m *MongoSource) tail(ctx context.Context, runID string, fromSeq int64, sub *mongoSubscription) {
	defer close(sub.events)
	defer close(sub.errors)

	const baseBackoff = 250 * time.Millisecond
	const maxBackoff = 30 * time.Second
	backoff := baseBackoff

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		nextSeq, err := m.runChangeStream(ctx, runID, fromSeq, sub)
		if err == nil {
			// runChangeStream returned because ctx is done — exit.
			return
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		// Surface the error (non-fatal) and reconnect.
		select {
		case sub.errors <- fmt.Errorf("eventstream/mongo: stream (will reconnect in %s): %w", backoff, err):
		default:
			// errors channel saturated — drop oldest by skipping.
			m.logger.Warn("eventstream/mongo: errors channel full; dropping reconnect notice")
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

// runChangeStream opens the change-stream and pumps events into the
// subscription. Returns the highest seq observed so a reconnect can
// resume past it. A nil error means ctx was cancelled cleanly; a
// non-nil error means the stream failed and the caller should
// backoff + reconnect.
func (m *MongoSource) runChangeStream(ctx context.Context, runID string, fromSeq int64, sub *mongoSubscription) (int64, error) {
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.M{
			"operationType":       "insert",
			"fullDocument.run_id": runID,
			"fullDocument.seq":    bson.M{"$gt": fromSeq},
		}}},
	}
	stream, err := m.events.Watch(ctx, pipeline, options.ChangeStream().SetFullDocument(options.UpdateLookup))
	if err != nil {
		return fromSeq, fmt.Errorf("open change stream: %w", err)
	}
	defer stream.Close(ctx)

	maxSeq := fromSeq
	for stream.Next(ctx) {
		var doc struct {
			FullDocument store.Event `bson:"fullDocument"`
		}
		if err := stream.Decode(&doc); err != nil {
			// A single bad doc is not fatal — log and continue
			// rather than tearing down the whole stream.
			select {
			case sub.errors <- fmt.Errorf("eventstream/mongo: decode change: %w", err):
			default:
			}
			continue
		}
		select {
		case sub.events <- &doc.FullDocument:
		case <-ctx.Done():
			return maxSeq, nil
		}
		if doc.FullDocument.Seq > maxSeq {
			maxSeq = doc.FullDocument.Seq
		}
	}
	if err := stream.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return maxSeq, err
	}
	// Stream exited cleanly — usually means ctx was cancelled.
	return maxSeq, nil
}

type mongoSubscription struct {
	events chan *store.Event
	errors chan error
	cancel context.CancelFunc
}

func (s *mongoSubscription) Events() <-chan *store.Event { return s.events }
func (s *mongoSubscription) Errors() <-chan error        { return s.errors }
func (s *mongoSubscription) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

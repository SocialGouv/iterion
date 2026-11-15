package runstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MongoSelection is implemented structurally by the store's immutable selector.
// Backfill callbacks occur after committed snapshots and must not be replayed by
// a transaction retry. Collections and the client remain borrowed.
type MongoSelection interface {
	EventsCollection() *mongo.Collection
	LogsCollection() *mongo.Collection
	EventFilter() bson.D
	LogFilter() bson.D
	Validate(context.Context) error
	DecodeEvent(context.Context, bson.Raw) (*store.Event, error)
	BackfillEvents(context.Context, int64, bool, int, func([]*store.Event) bool) error
	BackfillLogs(context.Context, int64, func(int64, []byte) bool) error
	Terminal(context.Context) (bool, error)
}
type MongoResolver func(context.Context, string) (MongoSelection, error)

var errMongoSourceClosed = errors.New("runstream: Mongo source is closed")

func NewMongoResolved(resolve MongoResolver, logger *iterlog.Logger) *MongoSource {
	m := NewMongo(nil, nil, nil, logger)
	m.resolver = resolve
	m.resolved = true
	return m
}

func (m *MongoSource) register(ctx context.Context) (context.Context, context.CancelFunc, <-chan struct{}, func(), error) {
	if ctx == nil {
		return nil, nil, nil, nil, store.ErrInvalidValueFormat
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, nil, err
	}
	c, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return nil, nil, nil, nil, errMongoSourceClosed
	}
	if m.active == nil {
		m.active = make(map[uint64]context.CancelFunc)
	}
	m.next++
	id := m.next
	m.active[id] = cancel
	m.workers.Add(1)
	m.mu.Unlock()
	done := make(chan struct{})
	var once sync.Once
	finish := func() {
		once.Do(func() {
			cancel()
			m.mu.Lock()
			delete(m.active, id)
			m.mu.Unlock()
			close(done)
			m.workers.Done()
		})
	}
	return c, cancel, done, finish, nil
}
func (m *MongoSource) selectStream(ctx context.Context, id string) (MongoSelection, error) {
	if m.resolved {
		if m.resolver == nil {
			return nil, store.ErrRunDataCapability
		}
		selection, err := m.resolver(ctx, id)
		if err != nil {
			return nil, err
		}
		if nilEventSelection(selection) {
			return nil, store.ErrRunDataCapability
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		return selection, nil
	}
	if _, ok := store.RunDataBindingFrom(ctx); ok {
		return nil, store.ErrRunDataCapability
	}
	f, err := store.ValueFormatForWrite(ctx, id)
	if err != nil {
		return nil, err
	}
	if f != store.LegacyValueFormat {
		return nil, store.ErrValueFormatCapability
	}
	return nil, nil
}
func mongoPermanent(err error) bool { return IsPermanentError(err) }

func mongoLogEnd(off int64, data []byte) (int64, error) {
	if off < 0 || int64(len(data)) > math.MaxInt64-off {
		return 0, fmt.Errorf("%w: invalid raw log extent", store.ErrEventsCorrupted)
	}
	return off + int64(len(data)), nil
}

type mongoEventPosition struct {
	seq   int64
	after bool
}

func (p mongoEventPosition) bounds() bson.M {
	if p.after {
		return bson.M{"$gt": p.seq}
	}
	return bson.M{"$gte": p.seq}
}
func (p mongoEventPosition) contains(seq int64) bool {
	if p.after {
		return seq > p.seq
	}
	return seq >= p.seq
}
func mongoWatchFilter(base bson.D, extra bson.M) bson.D {
	scope := bson.D{{Key: "operationType", Value: "insert"}}
	for _, entry := range base {
		scope = append(scope, bson.E{Key: "fullDocument." + entry.Key, Value: entry.Value})
	}
	if len(extra) > 0 {
		return bson.D{{Key: "$and", Value: bson.A{scope, extra}}}
	}
	return scope
}

// A round has one poller and one producer. Only the producer publishes errors.
// Joining exposes a refusal even when cancellation caused Ship/Next to return.
type mongoRound struct {
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	terminal bool
	err      error
}

func newMongoRound(ctx context.Context, selection MongoSelection) *mongoRound {
	c, cancel := context.WithCancel(ctx)
	r := &mongoRound{ctx: c, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(mongoTerminalPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-c.Done():
				return
			case <-ticker.C:
				terminal, err := selection.Terminal(c)
				if terminal || err != nil {
					r.terminal = terminal
					r.err = err
					cancel()
					return
				}
			}
		}
	}()
	return r
}
func (r *mongoRound) finish(parent context.Context, err error) (bool, error) {
	r.cancel()
	<-r.done
	// Internal round cancellation is transport control, not a clean refusal.
	if mongoContextErrorOnly(err) && errors.Is(err, context.Canceled) && parent.Err() == nil {
		err = nil
	}
	pollErr := r.err
	if mongoContextErrorOnly(pollErr) && errors.Is(pollErr, context.Canceled) && parent.Err() == nil {
		pollErr = nil
	}
	return r.terminal, errors.Join(err, pollErr)
}
func mongoRetry(ctx context.Context, err error, delay time.Duration, warn func(error)) bool {
	if ctx.Err() != nil {
		return false
	}
	warn(fmt.Errorf("runstream/mongo: reconnect in %s: %w", delay, err))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func nextMongoDelay(d time.Duration) time.Duration {
	if d >= 15*time.Second {
		return 30 * time.Second
	}
	return d * 2
}
func (m *MongoSource) runSelectedEvents(ctx context.Context, s MongoSelection, pos mongoEventPosition, pipe EventPipe) {
	defer pipe.Finish(nil)
	original, observed, err := store.CaptureEventStreamObservation(s)
	if err != nil {
		pipe.Fail(err)
		return
	}
	delay := 250 * time.Millisecond
	replay := func(c context.Context) error { return replayObservedEvents(c, s, &pos, pipe, original, observed) }
	for {
		terminal, err := m.selectedEventsOnce(ctx, s, &pos, pipe, replay, original, observed)
		if mongoPermanent(err) {
			pipe.Fail(err)
			return
		}
		if ctx.Err() != nil || errors.Is(err, errEventDeliveryStopped) {
			return
		}
		if terminal && err == nil {
			err = replay(ctx)
			if mongoPermanent(err) {
				pipe.Fail(err)
				return
			}
			if err == nil || errors.Is(err, errEventDeliveryStopped) {
				return
			}
		}
		if err == nil {
			err = io.EOF
		}
		if !mongoRetry(ctx, err, delay, pipe.Warn) {
			return
		}
		delay = nextMongoDelay(delay)
	}
}
func (m *MongoSource) selectedEventsOnce(ctx context.Context, s MongoSelection, pos *mongoEventPosition, pipe EventPipe, replay func(context.Context) error, original store.RunObservation, observed bool) (terminal bool, err error) {
	round := newMongoRound(ctx, s)
	defer func() { t, e := round.finish(ctx, err); terminal = terminal || t; err = e }()
	stream, err := s.EventsCollection().Watch(round.ctx, mongo.Pipeline{bson.D{{Key: "$match", Value: mongoWatchFilter(s.EventFilter(), bson.M{"fullDocument.seq": pos.bounds()})}}}, options.ChangeStream().SetFullDocument(options.UpdateLookup))
	if err != nil {
		return false, err
	}
	defer stream.Close(ctx)
	if err = replay(round.ctx); err != nil {
		return false, err
	}
	if terminal, err = s.Terminal(round.ctx); err != nil || terminal {
		return terminal, err
	}
	for stream.Next(round.ctx) {
		raw, ok := stream.Current.Lookup("fullDocument").DocumentOK()
		if !ok {
			return false, store.ErrEventsCorrupted
		}
		if err = s.Validate(round.ctx); err != nil {
			return false, err
		}
		evt, e := s.DecodeEvent(round.ctx, raw)
		if e != nil {
			return false, e
		}
		if e = checkObservedBatch([]*store.Event{evt}, original, observed); e != nil {
			return false, e
		}
		if !pos.contains(evt.Seq) {
			continue
		}
		if !pipe.Ship(round.ctx, []*store.Event{evt}) {
			return false, eventShipRefused(pipe, round.ctx)
		}
		*pos = mongoEventPosition{seq: evt.Seq, after: true}
		if m.metrics != nil && !evt.Timestamp.IsZero() {
			m.metrics.MongoChangeStreamLagS.Set(time.Since(evt.Timestamp).Seconds())
		}
	}
	return false, stream.Err()
}
func (m *MongoSource) runSelectedLogs(ctx context.Context, s MongoSelection, from int64, pipe LogPipe) {
	defer pipe.Finish(nil)
	delay := 250 * time.Millisecond
	replay := func(c context.Context) error {
		var callbackErr error
		err := s.BackfillLogs(c, from, func(off int64, data []byte) bool {
			if _, callbackErr = mongoLogEnd(off, data); callbackErr != nil {
				return false
			}
			var ok bool
			from, ok = ShipLogChunk(c, pipe, off, append([]byte(nil), data...), from)
			return ok
		})
		return errors.Join(err, callbackErr)
	}
	for {
		terminal, err := m.selectedLogsOnce(ctx, s, &from, pipe, replay)
		if mongoPermanent(err) {
			pipe.Fail(err)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if terminal && err == nil {
			err = replay(ctx)
			if mongoPermanent(err) {
				pipe.Fail(err)
				return
			}
			if err == nil {
				return
			}
		}
		if err == nil {
			err = io.EOF
		}
		if !mongoRetry(ctx, err, delay, pipe.Warn) {
			return
		}
		delay = nextMongoDelay(delay)
	}
}
func (m *MongoSource) selectedLogsOnce(ctx context.Context, s MongoSelection, from *int64, pipe LogPipe, replay func(context.Context) error) (terminal bool, err error) {
	round := newMongoRound(ctx, s)
	defer func() { t, e := round.finish(ctx, err); terminal = terminal || t; err = e }()
	stream, err := s.LogsCollection().Watch(round.ctx, mongo.Pipeline{bson.D{{Key: "$match", Value: mongoWatchFilter(s.LogFilter(), nil)}}}, options.ChangeStream().SetFullDocument(options.UpdateLookup))
	if err != nil {
		return false, err
	}
	defer stream.Close(ctx)
	if err = replay(round.ctx); err != nil {
		return false, err
	}
	if terminal, err = s.Terminal(round.ctx); err != nil || terminal {
		return terminal, err
	}
	for stream.Next(round.ctx) {
		raw, ok := stream.Current.Lookup("fullDocument").DocumentOK()
		if !ok {
			return false, store.ErrEventsCorrupted
		}
		if err = s.Validate(round.ctx); err != nil {
			return false, err
		}
		var doc runLogChunkDoc
		if err = bson.Unmarshal(raw, &doc); err != nil {
			return false, errors.Join(store.ErrEventsCorrupted, err)
		}
		if _, err = mongoLogEnd(doc.Offset, doc.Data); err != nil {
			return false, err
		}
		var shipped bool
		*from, shipped = ShipLogChunk(round.ctx, pipe, doc.Offset, append([]byte(nil), doc.Data...), *from)
		if !shipped {
			return false, round.ctx.Err()
		}
	}
	return false, stream.Err()
}

func mongoContextErrorOnly(err error) bool {
	if err == nil {
		return false
	}
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		parts := many.Unwrap()
		if len(parts) == 0 {
			return false
		}
		for _, part := range parts {
			if !mongoContextErrorOnly(part) {
				return false
			}
		}
		return true
	}
	if one, ok := err.(interface{ Unwrap() error }); ok {
		return mongoContextErrorOnly(one.Unwrap())
	}
	return err == context.Canceled || err == context.DeadlineExceeded
}

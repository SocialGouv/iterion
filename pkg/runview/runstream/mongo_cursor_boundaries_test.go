package runstream

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCursorMongoPublicMaxLifecycle(t *testing.T) {
	old := mongoTerminalPollInterval
	mongoTerminalPollInterval = 20 * time.Millisecond
	defer func() { mongoTerminalPollInterval = old }()
	for _, kind := range []string{"legacy", "selected1", "selected2", "strict1", "strict2"} {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprint(kind, "-", after), func(t *testing.T) {
				st, ctx := resolvedMongoFixture(t)
				f := store.LegacyValueFormat
				if strings.HasSuffix(kind, "2") {
					f = store.ExactValueFormat
				}
				protocol := store.HistoricalRunData
				if strings.HasPrefix(kind, "strict") {
					protocol = store.IncarnationRunData
					if e := store.RequireRunDataStorage(protocol); e != nil {
						if errors.Is(e, store.ErrRunDataCapability) {
							t.Skip("admission OFF")
						}
						t.Fatal(e)
					}
				}
				bound, r := resolvedBirth(t, st, ctx, f, protocol)
				if _, e := st.AppendEvent(bound, r.ID, store.Event{Type: store.EventNodeStarted}); e != nil {
					t.Fatal(e)
				}
				selection, e := st.ResolveStreamSelection(ctx, r.ID)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = selection.EventsCollection().UpdateMany(ctx, selection.EventFilter(), bson.M{"$set": bson.M{"seq": int64(math.MaxInt64)}}); e != nil {
					t.Fatal(e)
				}
				var src *MongoSource
				if kind == "legacy" {
					src = NewMongo(st.EventsCollection(), st.RunLogsCollection(), st.RunsCollection(), quietLogger())
				} else {
					src = NewMongoResolved(func(c context.Context, id string) (MongoSelection, error) { return st.ResolveStreamSelection(c, id) }, quietLogger())
				}
				defer src.Close()
				sub, e := src.SubscribeEventsCursor(ctx, r.ID, EventCursor{Seq: math.MaxInt64, After: after})
				if e != nil {
					t.Fatal(e)
				}
				defer sub.Close()
				if !after {
					ev := takeResolvedEvent(t, sub)
					if ev.Seq != math.MaxInt64 {
						t.Fatal(ev)
					}
				}
				select {
				case rows, ok := <-sub.Events():
					t.Fatalf("live tail ended or duplicate: %v %v", rows, ok)
				case e := <-sub.Errors():
					t.Fatal(e)
				case <-time.After(80 * time.Millisecond):
				}
				if e = st.UpdateRunStatus(bound, r.ID, store.RunStatusFinished, ""); e != nil {
					t.Fatal(e)
				}
				select {
				case rows, ok := <-sub.Events():
					if ok {
						t.Fatalf("terminal duplicate %v", rows)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("terminal not joined")
				}
			})
		}
	}
}

func TestCursorMongoSelectedRetryAfterMax(t *testing.T) {
	st, ctx := resolvedMongoFixture(t)
	bound, r := resolvedBirth(t, st, ctx, 1, 0)
	if _, e := st.AppendEvent(bound, r.ID, store.Event{}); e != nil {
		t.Fatal(e)
	}
	real, e := st.ResolveStreamSelection(ctx, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = real.EventsCollection().UpdateMany(ctx, real.EventFilter(), bson.M{"$set": bson.M{"seq": int64(math.MaxInt64)}}); e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	seen := make(chan mongoEventPosition, 4)
	wrapped := &runtimeBoundarySelection{MongoSelection: real, terminal: func(context.Context) (bool, error) { return false, nil }}
	wrapped.backEvents = func(c context.Context, n int64, a bool, b int, emit func([]*store.Event) bool) error {
		seen <- mongoEventPosition{seq: n, after: a}
		e := real.BackfillEvents(c, n, a, b, emit)
		if e == nil && calls.Add(1) == 1 {
			return errors.New("controlled retry after committed Max delivery")
		}
		return e
	}
	src := NewMongoResolved(func(context.Context, string) (MongoSelection, error) { return wrapped, nil }, quietLogger())
	defer src.Close()
	sub, e := src.SubscribeEventsCursor(ctx, r.ID, EventCursor{Seq: math.MaxInt64})
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Close()
	select {
	case rows := <-sub.Events():
		if len(rows) != 1 || rows[0].Seq != math.MaxInt64 {
			t.Fatal("max missing", rows)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("max missing")
	}
	for i := 0; i < 2; i++ {
		select {
		case p := <-seen:
			if p.seq != math.MaxInt64 || p.after != (i == 1) {
				t.Fatalf("retry %d position %+v", i, p)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("retry not observed")
		}
	}
	select {
	case rows := <-sub.Events():
		t.Fatalf("retry duplicate %+v", rows)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestCursorMongoLegacyRoundPosition(t *testing.T) {
	st, src, ctx := newMongoFixture(t)
	defer src.Close()
	if _, e := st.CreateRun(ctx, "r", "wf", nil); e != nil {
		t.Fatal(e)
	}
	if _, e := st.AppendEvent(ctx, "r", store.Event{}); e != nil {
		t.Fatal(e)
	}
	if _, e := st.EventsCollection().UpdateMany(ctx, bson.M{"run_id": "r"}, bson.M{"$set": bson.M{"seq": int64(math.MaxInt64)}}); e != nil {
		t.Fatal(e)
	}
	c, cancel := context.WithCancel(ctx)
	p := NewEventPipe()
	done := make(chan mongoEventPosition, 1)
	go func() {
		pos, _, _ := src.streamEventsOnce(c, "r", mongoEventPosition{seq: math.MaxInt64}, p)
		done <- pos
	}()
	select {
	case rows := <-p.Events():
		if len(rows) != 1 || rows[0].Seq != math.MaxInt64 {
			t.Fatal(rows)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("no backfill")
	}
	cancel()
	var pos mongoEventPosition
	select {
	case pos = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("round not joined")
	}
	p.Close()
	if pos != (mongoEventPosition{seq: math.MaxInt64, after: true}) {
		t.Fatalf("round resume %+v", pos)
	}
	next := NewEventPipe()
	defer next.Close()
	got, e := src.backfillEvents(ctx, "r", pos, next)
	if e != nil || got != pos {
		t.Fatal(got, e)
	}
	select {
	case rows := <-next.Events():
		t.Fatalf("next round duplicate %+v", rows)
	default:
	}
}

func TestCursorMongoMinEmptyThenZero(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(fmt.Sprint(selected), func(t *testing.T) {
			st, ctx := resolvedMongoFixture(t)
			bound, r := resolvedBirth(t, st, ctx, 1, 0)
			var src *MongoSource
			if selected {
				src = NewMongoResolved(func(c context.Context, id string) (MongoSelection, error) { return st.ResolveStreamSelection(c, id) }, quietLogger())
			} else {
				src = NewMongo(st.EventsCollection(), st.RunLogsCollection(), st.RunsCollection(), quietLogger())
			}
			defer src.Close()
			sub, e := src.SubscribeEventsCursor(ctx, r.ID, EventCursor{Seq: math.MinInt64})
			if e != nil {
				t.Fatal(e)
			}
			defer sub.Close()
			select {
			case rows, ok := <-sub.Events():
				t.Fatalf("empty must remain live %v %v", rows, ok)
			case <-time.After(50 * time.Millisecond):
			}
			if _, e = st.AppendEvent(bound, r.ID, store.Event{}); e != nil {
				t.Fatal(e)
			}
			if ev := takeResolvedEvent(t, sub); ev.Seq != 0 {
				t.Fatal(ev)
			}
		})
	}
}

func TestCursorMongoLegacyFailedShipDoesNotAdvance(t *testing.T) {
	st, src, ctx := newMongoFixture(t)
	defer src.Close()
	if _, e := st.CreateRun(ctx, "r", "wf", nil); e != nil {
		t.Fatal(e)
	}
	if _, e := st.AppendEvent(ctx, "r", store.Event{}); e != nil {
		t.Fatal(e)
	}
	p := NewEventPipe()
	for i := 0; i < cap(p.ch); i++ {
		p.ch <- nil
	}
	p.Close()
	start := mongoEventPosition{seq: math.MinInt64}
	next, _ := src.backfillEvents(ctx, "r", start, p)
	if next != start {
		t.Fatalf("unshipped cursor advanced %+v", next)
	}
}

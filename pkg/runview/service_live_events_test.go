package runview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newLiveEventsService builds a Service over the native filesystem store, the
// store whose StreamSource events are served by retained per-client replay.
// Recovery is passive so no reconcile pass reaps the running runs the tests
// write themselves.
func newLiveEventsService(t *testing.T, opts ...ServiceOption) (*Service, *store.FilesystemRunStore, string) {
	t.Helper()
	dir := t.TempDir()
	all := append([]ServiceOption{WithLogger(iterlog.Nop()), WithRecoveryPassive()}, opts...)
	svc, err := NewService(dir, all...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Stop(ctx)
	})
	native, ok := svc.store.(*store.FilesystemRunStore)
	if !ok {
		t.Fatalf("store is %T, want the native filesystem store", svc.store)
	}
	return svc, native, dir
}

// moveRunAside renames runID's directory out of the runs tree, inside the
// test's own directory, so a successor can take the ID while its predecessor
// is still running.
func moveRunAside(t *testing.T, dir, runID string) {
	t.Helper()
	if err := os.Rename(filepath.Join(dir, "runs", runID), filepath.Join(dir, "aside-"+runID)); err != nil {
		t.Fatalf("move run aside: %v", err)
	}
}

// valueCtx returns ctx bound to runID's value format, the context an append
// to a format-2 run requires.
func valueCtx(t *testing.T, st *store.FilesystemRunStore, runID string) context.Context {
	t.Helper()
	r, err := st.LoadRun(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := store.BindRunValueFormat(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// awaitEvents reads batches until pred holds; a stream that ends first fails
// the test instead of returning a short delivery.
func awaitEvents(t *testing.T, sub runstream.EventSubscription, d time.Duration, pred func([]*store.Event) bool) []*store.Event {
	t.Helper()
	got := drainEventBatches(t, sub, nil, d, pred)
	if !pred(got) {
		t.Fatalf("the stream ended before the expected delivery; got %+v", got)
	}
	return got
}

// reachLive appends then publishes n Data-less events (the in-process order),
// each awaited, so the subscription is past its replay before the caller's
// next step. Returns the seq of the last one.
func reachLive(t *testing.T, svc *Service, st *store.FilesystemRunStore, sub runstream.EventSubscription, n int) int64 {
	t.Helper()
	var last int64
	for i := 0; i < n; i++ {
		persisted, err := st.AppendEvent(valueCtx(t, st, "r"), "r", store.Event{Type: store.EventNodeFinished, NodeID: "live"})
		if err != nil {
			t.Fatal(err)
		}
		svc.broker.Publish(*persisted)
		got := awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
		if len(got) != 1 || got[0].Seq != persisted.Seq {
			t.Fatalf("live step delivered %+v, want seq %d once", got, persisted.Seq)
		}
		last = persisted.Seq
	}
	return last
}

// expectNoEvent fails when the subscription delivers an event within d.
func expectNoEvent(t *testing.T, sub runstream.EventSubscription, d time.Duration, what string) {
	t.Helper()
	select {
	case batch, ok := <-sub.Events():
		if !ok {
			t.Fatalf("%s: the stream ended", what)
		}
		t.Fatalf("%s: delivered %+v", what, batch)
	case <-time.After(d):
	}
}

// T1: a successor's persisted event published on the run ID never reaches a
// subscription bound to its predecessor; the subscription fails with the
// identity refusal instead.
func TestLiveEventsSuccessorPublicationNeverDelivered(t *testing.T) {
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprintf("f%d", f), func(t *testing.T) {
			svc, native, dir := newLiveEventsService(t)
			ctx := t.Context()
			if err := seedOriginRun(ctx, native, "r", "A", f, store.RunStatusRunning, 1); err != nil {
				t.Fatal(err)
			}
			bound := boundOriginCtx(t, svc, "r")
			sub, err := runstream.SubscribeEventsCursor(bound, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			last := reachLive(t, svc, native, sub, 2)
			moveRunAside(t, dir, "r")
			if err := seedOriginRun(ctx, native, "r", "B", f, store.RunStatusRunning, int(last)+2); err != nil {
				t.Fatal(err)
			}
			successor, err := native.LoadEvents(ctx, "r")
			if err != nil {
				t.Fatal(err)
			}
			if len(successor) != int(last)+2 || successor[last+1].NodeID != "B" {
				t.Fatalf("successor events = %+v", successor)
			}
			// Past the predecessor's cursor: only the run's identity can refuse it.
			svc.broker.Publish(*successor[last+1])
			got, errs := drainOriginSubscription(t, sub)
			for _, ev := range got {
				if ev.NodeID == "B" {
					t.Fatalf("successor event delivered: %+v", ev)
				}
			}
			if !hasError(errs, store.ErrRunDataIdentityChanged) {
				t.Fatalf("errors = %v, want the identity refusal", errs)
			}
		})
	}
}

// T1 positive control: without a replacement, an event appended and then
// published (the in-process order) arrives exactly once.
func TestLiveEventsAppendedAndPublishedArrivesOnce(t *testing.T) {
	svc, native, _ := newLiveEventsService(t)
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	bound := boundOriginCtx(t, svc, "r")
	sub, err := runstream.SubscribeEventsCursor(bound, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	persisted, err := native.AppendEvent(valueCtx(t, native, "r"), "r", store.Event{Type: store.EventNodeFinished, NodeID: "A2"})
	if err != nil {
		t.Fatal(err)
	}
	svc.broker.Publish(*persisted)
	got := awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
	if len(got) != 1 || got[0].Seq != 1 || got[0].NodeID != "A2" {
		t.Fatalf("delivered %+v, want seq 1 A2 once", got)
	}
	expectNoEvent(t, sub, 500*time.Millisecond, "duplicate delivery")
}

// T3: a broker payload is never shipped on the native path. An unpersisted
// non-alert publication is not delivered; the persisted row with the same
// seq is, once, from disk.
func TestLiveEventsBrokerPayloadNeverShipped(t *testing.T) {
	svc, native, _ := newLiveEventsService(t)
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	bound := boundOriginCtx(t, svc, "r")
	sub, err := runstream.SubscribeEventsCursor(bound, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	svc.broker.Publish(store.Event{Seq: 1, Type: store.EventNodeFinished, RunID: "r", NodeID: "BROKER"})
	expectNoEvent(t, sub, 500*time.Millisecond, "unpersisted broker payload shipped")
	if _, err := native.AppendEvent(valueCtx(t, native, "r"), "r", store.Event{Type: store.EventNodeFinished, NodeID: "DISK"}); err != nil {
		t.Fatal(err)
	}
	got := awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
	if len(got) != 1 || got[0].Seq != 1 || got[0].NodeID != "DISK" {
		t.Fatalf("delivered %+v, want the persisted seq 1 DISK once", got)
	}
	expectNoEvent(t, sub, 300*time.Millisecond, "second delivery")
}

// T7: an exact number appended to a running format-2 run this process does
// not produce is delivered live, exactly as written.
func TestLiveEventsExternalExactNumberDeliveredLive(t *testing.T) {
	svc, native, _ := newLiveEventsService(t)
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.ExactValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	bound := boundOriginCtx(t, svc, "r")
	sub, err := runstream.SubscribeEventsCursor(bound, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	reachLive(t, svc, native, sub, 2)
	if _, err := native.AppendEvent(valueCtx(t, native, "r"), "r", store.Event{Type: store.EventNodeFinished, NodeID: "exact", Data: map[string]any{"n": json.Number("1e400")}}); err != nil {
		t.Fatal(err)
	}
	got := awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
	if got[0].NodeID != "exact" {
		t.Fatalf("delivered %+v, want the exact event", got[0])
	}
	wire, err := json.Marshal(got[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != `{"n":1e400}` {
		t.Fatalf("data on the wire = %s, want {\"n\":1e400}", wire)
	}
}

// liveRegistered returns how many merges the live registry holds.
func liveRegistered(svc *Service) int {
	svc.liveEvents.mu.Lock()
	defer svc.liveEvents.mu.Unlock()
	return len(svc.liveEvents.active)
}

// tailerAttached reports whether an ID-keyed events tailer runs for runID.
func tailerAttached(svc *Service, runID string) bool {
	svc.fileSrcMu.Lock()
	defer svc.fileSrcMu.Unlock()
	return svc.fileSrcs[runID] != nil
}

// requireReleased asserts, without waiting, that nothing a subscription of
// runID held is still registered.
func requireReleased(t *testing.T, svc *Service, runID, when string) {
	t.Helper()
	if n := svc.broker.SubscriberCount(runID); n != 0 {
		t.Fatalf("%s: %d broker subscribers remain", when, n)
	}
	if tailerAttached(svc, runID) {
		t.Fatalf("%s: the events tailer is still attached", when)
	}
	if n := liveRegistered(svc); n != 0 {
		t.Fatalf("%s: %d live streams remain registered", when, n)
	}
}

// T1b: the binding reaches the retained subscription: bound to a run already
// replaced before subscribing, nothing of the successor is delivered.
func TestLiveEventsBindingReachesTheRetainedSubscription(t *testing.T) {
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprintf("f%d", f), func(t *testing.T) {
			svc, native, dir := newLiveEventsService(t)
			ctx := t.Context()
			if err := seedOriginRun(ctx, native, "r", "A", f, store.RunStatusRunning, 1); err != nil {
				t.Fatal(err)
			}
			bound := boundOriginCtx(t, svc, "r")
			moveRunAside(t, dir, "r")
			if err := seedOriginRun(ctx, native, "r", "B", f, store.RunStatusRunning, 2); err != nil {
				t.Fatal(err)
			}
			sub, err := runstream.SubscribeEventsCursor(bound, svc.StreamSource(), "r", runstream.EventCursor{Seq: math.MinInt64})
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			got, errs := drainOriginSubscription(t, sub)
			if len(got) != 0 {
				t.Fatalf("delivered %+v from a replaced run", got)
			}
			if !hasError(errs, store.ErrRunDataIdentityChanged) {
				t.Fatalf("errors = %v, want the identity refusal", errs)
			}
		})
	}
}

// T2a: an alert (Seq 0, unpersisted) reaches a subscription whose cursor is
// past 0, out of band.
func TestLiveEventsAlertBypassesTheCursor(t *testing.T) {
	svc, _ := newStreamTestService(t)
	seedStreamRun(t, svc, "r", 3)
	sub, err := runstream.SubscribeEventsCursor(t.Context(), svc.StreamSource(), "r", runstream.EventCursor{Seq: 2, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	svc.broker.Publish(store.Event{RunID: "r", Type: store.EventAlert, NodeID: "n"})
	got := awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
	if len(got) != 1 || got[0].Type != store.EventAlert {
		t.Fatalf("delivered %+v, want the alert", got)
	}
}

// T5: CloseRun ends an in-process stream at once, after its final event,
// whatever the terminal poll interval.
func TestLiveEventsCloseRunEndsTheStream(t *testing.T) {
	prev := svcEventTerminalPollInterval
	svcEventTerminalPollInterval = time.Hour
	t.Cleanup(func() { svcEventTerminalPollInterval = prev })
	svc, native, _ := newLiveEventsService(t)
	if err := seedOriginRun(t.Context(), native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	sub, err := runstream.SubscribeEventsCursor(t.Context(), svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	reachLive(t, svc, native, sub, 1)
	// The retained subscription's immediate first terminal check has run long
	// before this: from here on only CloseRun can trigger one.
	time.Sleep(100 * time.Millisecond)
	// The engine's order: terminal status, then the terminal event.
	if err := native.UpdateRunStatus(t.Context(), "r", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := native.AppendEvent(t.Context(), "r", store.Event{Type: store.EventRunFinished}); err != nil {
		t.Fatal(err)
	}
	got := awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
	if len(got) != 1 || got[0].Type != store.EventRunFinished {
		t.Fatalf("delivered %+v, want run_finished", got)
	}
	expectNoEvent(t, sub, 300*time.Millisecond, "the stream ended without CloseRun")
	svc.broker.CloseRun("r")
	got, errs := drainEventsWithin(t, sub, 5*time.Second)
	if len(errs) != 0 || len(got) != 0 {
		t.Fatalf("after CloseRun: events %+v, errors %v, want a clean end", got, errs)
	}
}

// drainEventsWithin reads a subscription to its end within d.
func drainEventsWithin(t *testing.T, sub runstream.EventSubscription, d time.Duration) ([]*store.Event, []error) {
	t.Helper()
	var got []*store.Event
	var errs []error
	events, errc := sub.Events(), sub.Errors()
	deadline := time.After(d)
	for events != nil || errc != nil {
		select {
		case batch, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			got = append(got, batch...)
		case err, ok := <-errc:
			if !ok {
				errc = nil
				continue
			}
			errs = append(errs, err)
		case <-deadline:
			t.Fatalf("subscription did not end within %v; got %d events", d, len(got))
		}
	}
	return got, errs
}

// T6: cursor boundaries on the retained path.
func TestLiveEventsCursorMatrix(t *testing.T) {
	cases := []struct {
		name   string
		cursor runstream.EventCursor
		want   []int64
	}{
		{"inclusive", runstream.EventCursor{Seq: 2}, []int64{2, 3, 4}},
		{"exclusive", runstream.EventCursor{Seq: 2, After: true}, []int64{3, 4}},
		{"min", runstream.EventCursor{Seq: math.MinInt64}, []int64{0, 1, 2, 3, 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newStreamTestService(t)
			seedStreamRun(t, svc, "r", 5)
			sub, err := runstream.SubscribeEventsCursor(t.Context(), svc.StreamSource(), "r", tc.cursor)
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			got := awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= len(tc.want) })
			for i, ev := range got {
				if i >= len(tc.want) || ev.Seq != tc.want[i] {
					t.Fatalf("delivered seqs %v, want %v", seqsOf(got), tc.want)
				}
			}
			expectNoEvent(t, sub, 200*time.Millisecond, "delivery past the expected window")
		})
	}
	t.Run("empty-run", func(t *testing.T) {
		svc, _ := newStreamTestService(t)
		seedStreamRun(t, svc, "r", 0)
		sub, err := runstream.SubscribeEventsCursor(t.Context(), svc.StreamSource(), "r", runstream.EventCursor{Seq: 0})
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
		expectNoEvent(t, sub, 200*time.Millisecond, "an event before any append")
		if _, err := svc.store.AppendEvent(t.Context(), "r", store.Event{Type: store.EventNodeStarted}); err != nil {
			t.Fatal(err)
		}
		got := awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
		if len(got) != 1 || got[0].Seq != 0 {
			t.Fatalf("delivered %v, want the first append", seqsOf(got))
		}
	})
	t.Run("concurrent-writer", func(t *testing.T) {
		svc, _ := newStreamTestService(t)
		seedStreamRun(t, svc, "r", 0)
		const n = 500
		written := make(chan error, 1)
		go func() {
			for i := 0; i < n; i++ {
				persisted, err := svc.store.AppendEvent(context.Background(), "r", store.Event{Type: store.EventNodeStarted})
				if err != nil {
					written <- err
					return
				}
				svc.broker.Publish(*persisted)
			}
			written <- nil
		}()
		sub, err := runstream.SubscribeEventsCursor(t.Context(), svc.StreamSource(), "r", runstream.EventCursor{Seq: math.MinInt64})
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
		got := awaitEvents(t, sub, 20*time.Second, func(evs []*store.Event) bool { return len(evs) >= n })
		if err := <-written; err != nil {
			t.Fatal(err)
		}
		for i, ev := range got {
			if ev.Seq != int64(i) {
				t.Fatalf("event %d has seq %d: duplicate or gap", i, ev.Seq)
			}
		}
		expectNoEvent(t, sub, 200*time.Millisecond, "delivery past the last append")
	})
}

func seqsOf(evs []*store.Event) []int64 {
	out := make([]int64, len(evs))
	for i, ev := range evs {
		out[i] = ev.Seq
	}
	return out
}

// T8a: StopBackground ends an open stream with ErrSourceClosed and has
// released everything it held when it returns; a later subscription is
// refused with ErrSourceClosed.
func TestLiveEventsStopBackgroundEndsAndReleases(t *testing.T) {
	svc, _ := newStreamTestService(t)
	seedStreamRun(t, svc, "r", 1)
	// The merge lingers before its cleanup: only a join can see it released.
	svc.liveEvents.beforeFinish = func(context.Context) { time.Sleep(200 * time.Millisecond) }
	sub, err := svc.StreamSource().SubscribeEvents(t.Context(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
	svc.StopBackground(t.Context())
	requireReleased(t, svc, "r", "after StopBackground")
	_, errs := drainEventsWithin(t, sub, time.Second)
	if !hasError(errs, runstream.ErrSourceClosed) {
		t.Fatalf("errors = %v, want ErrSourceClosed", errs)
	}
	if _, err := svc.StreamSource().SubscribeEvents(t.Context(), "r", 0); !errors.Is(err, runstream.ErrSourceClosed) {
		t.Fatalf("subscribe after stop = %v, want ErrSourceClosed", err)
	}
}

// T8b: a stream blocked delivering to a consumer that never reads is joined
// by a stop with a short budget.
func TestLiveEventsStopJoinsABlockedDelivery(t *testing.T) {
	defer runstream.SetMaxEventsPerPageForTest(1)()
	svc, _ := newStreamTestService(t)
	seedStreamRun(t, svc, "r", 40)
	sub, err := svc.StreamSource().SubscribeEvents(t.Context(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	// Forty one-event batches cannot fit the pipes: the merge blocks in Ship.
	deadline := time.Now().Add(5 * time.Second)
	for len(sub.Events()) < cap(sub.Events()) {
		if time.Now().After(deadline) {
			t.Fatal("the delivery buffer never filled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if err := svc.liveEvents.stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	requireReleased(t, svc, "r", "after a stop of a blocked delivery")
}

// T8c and F6: an already-expired stop still cancels, but a stream it could
// not join keeps its registration and the Service FileSource stays open; a
// later stop finishes the cleanup and closes the source.
func TestLiveEventsExpiredStopCancelsAndKeepsUnjoinedState(t *testing.T) {
	svc, _ := newStreamTestService(t)
	seedStreamRun(t, svc, "r", 1)
	hold, held := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	svc.liveEvents.beforeFinish = func(context.Context) {
		close(held)
		<-hold
	}
	sub, err := svc.StreamSource().SubscribeEvents(t.Context(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	// Runs first on a failed assertion: Close joins the held merge.
	defer release()
	expired, cancel := context.WithCancel(t.Context())
	cancel()
	if err := svc.liveEvents.stop(expired); err == nil {
		t.Fatal("an expired stop reported a join it could not observe")
	}
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the expired stop did not cancel the stream")
	}
	if n := liveRegistered(svc); n != 1 {
		t.Fatalf("unjoined streams registered = %d, want 1", n)
	}
	probe, err := svc.liveEvents.files.SubscribeEvents(t.Context(), "r", 0)
	if err != nil {
		t.Fatalf("the Service FileSource was closed before its streams joined: %v", err)
	}
	_ = probe.Close()
	release()
	if err := svc.liveEvents.stop(t.Context()); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	requireReleased(t, svc, "r", "after the second stop")
	if _, err := svc.liveEvents.files.SubscribeEvents(t.Context(), "r", 0); !errors.Is(err, runstream.ErrSourceClosed) {
		t.Fatalf("Service FileSource after a joined stop: %v, want ErrSourceClosed", err)
	}
	_, errs := drainEventsWithin(t, sub, time.Second)
	if !hasError(errs, runstream.ErrSourceClosed) {
		t.Fatalf("errors = %v, want ErrSourceClosed", errs)
	}
}

// T8d: the broker subscription exists when SubscribeEventsCursor returns, and
// Close returns only once everything the stream held is released.
func TestLiveEventsSubscribeAndCloseAreSynchronous(t *testing.T) {
	svc, _ := newStreamTestService(t)
	seedStreamRun(t, svc, "r", 1)
	// The merge lingers before its cleanup: only a join can see it released.
	svc.liveEvents.beforeFinish = func(context.Context) { time.Sleep(200 * time.Millisecond) }
	sub, err := runstream.SubscribeEventsCursor(t.Context(), svc.StreamSource(), "r", runstream.EventCursor{Seq: 0})
	if err != nil {
		t.Fatal(err)
	}
	if n := svc.broker.SubscriberCount("r"); n != 1 {
		t.Fatalf("broker subscribers at return = %d, want 1", n)
	}
	if !tailerAttached(svc, "r") {
		t.Fatal("no events tailer for a run this process does not produce")
	}
	if n := liveRegistered(svc); n != 1 {
		t.Fatalf("registered streams = %d, want 1", n)
	}
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	requireReleased(t, svc, "r", "after Close")
}

// T8e: Drain without an active run ends the streams, with ErrSourceClosed.
func TestLiveEventsDrainEndsStreams(t *testing.T) {
	svc, _ := newStreamTestService(t)
	seedStreamRun(t, svc, "r", 1)
	sub, err := svc.StreamSource().SubscribeEvents(t.Context(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	svc.Drain(t.Context())
	requireReleased(t, svc, "r", "after Drain")
	_, errs := drainEventsWithin(t, sub, 5*time.Second)
	if !hasError(errs, runstream.ErrSourceClosed) {
		t.Fatalf("errors = %v, want ErrSourceClosed", errs)
	}
}

// T8e variant (D2a): Drain ends the streams of the runs it drains after those
// runs' own ends. What a run persists while Drain waits for it is delivered,
// its own end (CloseRun after its terminal event) closes its stream clean, and
// only a stream of a run Drain did not drain ends with ErrSourceClosed.
func TestLiveEventsDrainEndsStreamsAfterTheRuns(t *testing.T) {
	svc, native, _ := newLiveEventsService(t)
	for _, id := range []string{"r", "other"} {
		if err := seedOriginRun(t.Context(), native, id, "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
			t.Fatal(err)
		}
	}
	// Every stream lingers before its end: only a Drain that waits for the
	// drained run's stream lets that stream end on its own.
	svc.liveEvents.beforeFinish = func(context.Context) { time.Sleep(200 * time.Millisecond) }
	cancelled, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	if err := svc.manager.RegisterDetached("r", 0, func() { once.Do(func() { close(cancelled) }) }, done); err != nil {
		t.Fatal(err)
	}
	// The fake run ends on every exit, a failed assertion included.
	finish := sync.OnceFunc(func() { close(done) })
	t.Cleanup(finish)
	bound := boundOriginCtx(t, svc, "r")
	sub, err := runstream.SubscribeEventsCursor(bound, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	other, err := runstream.SubscribeEventsCursor(t.Context(), svc.StreamSource(), "other", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		svc.Drain(context.Background())
	}()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not cancel the run")
	}
	persisted, err := native.AppendEvent(t.Context(), "r", store.Event{Type: store.EventNodeFinished, NodeID: "cancelling"})
	if err != nil {
		t.Fatal(err)
	}
	svc.broker.Publish(*persisted)
	got := awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
	if got[0].NodeID != "cancelling" {
		t.Fatalf("delivered %+v, want the event persisted while Drain waited", got)
	}
	// The in-process teardown order: terminal status, terminal event and its
	// publication, CloseRun, then the run's done.
	if err := native.UpdateRunStatus(t.Context(), "r", store.RunStatusCancelled, ""); err != nil {
		t.Fatal(err)
	}
	ended, err := native.AppendEvent(t.Context(), "r", store.Event{Type: store.EventRunCancelled})
	if err != nil {
		t.Fatal(err)
	}
	svc.broker.Publish(*ended)
	svc.broker.CloseRun("r")
	finish()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("Drain did not return")
	}
	rest, errs := drainEventsWithin(t, sub, 5*time.Second)
	if len(errs) != 0 {
		t.Fatalf("drained run's stream errors = %v, want its own clean end", errs)
	}
	sawCancelled := false
	for _, ev := range rest {
		sawCancelled = sawCancelled || ev.Type == store.EventRunCancelled
	}
	if !sawCancelled {
		t.Fatalf("drained run's stream delivered %v, want its run_cancelled", seqsOf(rest))
	}
	_, otherErrs := drainEventsWithin(t, other, 5*time.Second)
	if !hasError(otherErrs, runstream.ErrSourceClosed) {
		t.Fatalf("errors of a run Drain did not drain = %v, want ErrSourceClosed", otherErrs)
	}
}

// Once the retained subscription reported a permanent refusal, the merge reads
// no more alerts: alerts published after the refusal may be a successor's.
func TestLiveEventsMergeStopsTheAlertLaneOnceRefused(t *testing.T) {
	svc, _ := newStreamTestService(t)
	seedStreamRun(t, svc, "r", 1)
	l := svc.liveEvents
	inner := runstream.NewEventPipe()
	c, cancel := context.WithCancelCause(context.Background())
	entry := &liveEventEntry{cancel: cancel, done: make(chan struct{})}
	l.mu.Lock()
	l.next++
	id := l.next
	l.active[id] = entry
	l.mu.Unlock()
	m := &liveEventMerge{l: l, id: id, runID: "r", ctx: c, entry: entry, dataWake: make(chan struct{}, 1), termHint: make(chan struct{}, 1)}
	lane := svc.broker.Subscribe("r")
	m.broker, m.inner = lane, inner
	m.out = runstream.NewEventPipeWithClose(func() { cancel(context.Canceled); <-entry.done })
	defer m.out.Close()
	go func() {
		defer close(entry.done)
		defer m.out.Finish(m.cleanup)
		m.run()
	}()
	// The retained subscription's order on a refusal: Fail, later Finish.
	inner.Fail(store.ErrRunDataIdentityChanged)
	deadline := time.Now().Add(5 * time.Second)
	for len(inner.Errors()) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the merge never took the refusal")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		svc.broker.Publish(store.Event{RunID: "r", Type: store.EventAlert, NodeID: "successor-alert"})
	}
	// A merge still reading the lane takes them within this wait.
	for wait := time.Now().Add(300 * time.Millisecond); len(lane.C) > 0 && time.Now().Before(wait); {
		time.Sleep(time.Millisecond)
	}
	inner.Finish(nil)
	got, errs := drainEventsWithin(t, m.out, 5*time.Second)
	if len(got) != 0 {
		t.Fatalf("delivered %+v after the refusal", got)
	}
	if !hasError(errs, store.ErrRunDataIdentityChanged) {
		t.Fatalf("errors = %v, want the refusal", errs)
	}
}

// F3: a Service stop that lands after the retained subscription ended cleanly
// but before the merge finished still ends the stream with ErrSourceClosed.
func TestLiveEventsStopWinsOverACleanEnd(t *testing.T) {
	svc, _ := newStreamTestService(t)
	seedStreamRun(t, svc, "r", 1)
	if _, err := svc.store.AppendEvent(t.Context(), "r", store.Event{Type: store.EventRunFinished}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.UpdateRunStatus(t.Context(), "r", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	svc.liveEvents.beforeFinish = func(ctx context.Context) {
		go func() {
			defer close(stopped)
			_ = svc.liveEvents.stop(context.Background())
		}()
		<-ctx.Done()
	}
	sub, err := svc.StreamSource().SubscribeEvents(t.Context(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	got, errs := drainEventsWithin(t, sub, 10*time.Second)
	if len(got) != 2 {
		t.Fatalf("delivered %v, want the two persisted events", seqsOf(got))
	}
	if !hasError(errs, runstream.ErrSourceClosed) {
		t.Fatalf("errors = %v, want ErrSourceClosed over the clean end", errs)
	}
	<-stopped
}

// F5: an unbound subscription to a missing run is refused at once, not left
// waiting for the run to appear.
func TestLiveEventsUnboundMissingRunRefused(t *testing.T) {
	svc, _ := newStreamTestService(t)
	sub, err := svc.StreamSource().SubscribeEvents(t.Context(), "missing", 0)
	if sub != nil {
		_ = sub.Close()
	}
	if !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("subscribe = %v, want ErrRunNotFound", err)
	}
	if n := liveRegistered(svc); n != 0 {
		t.Fatalf("a refused subscription stayed registered: %d", n)
	}
}

// T10: event-log corruption past the threshold is fatal on the retained path,
// and the fatal error survives a consumer that left four warnings unread.
func TestLiveEventsCorruptionIsFatal(t *testing.T) {
	svc, dir := newStreamTestService(t)
	seedStreamRun(t, svc, "r", 0)
	path := filepath.Join(dir, "runs", "r", "events.jsonl")
	var lines []byte
	for i := 0; i < 20; i++ {
		line, err := json.Marshal(store.Event{Seq: int64(i), RunID: "r", Type: store.EventNodeStarted})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(append(lines, line...), '\n')
		if i%5 == 0 {
			lines = append(lines, "not json\n"...)
		}
	}
	if err := os.WriteFile(path, lines, 0o600); err != nil {
		t.Fatal(err)
	}
	sub, err := svc.StreamSource().SubscribeEvents(t.Context(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 20 })
	deadline := time.Now().Add(5 * time.Second)
	for len(sub.Errors()) < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("decode warnings queued = %d, want 4", len(sub.Errors()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if _, err := f.WriteString("still not json\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Errors stay unread until Events close: the buffer stays saturated, so
	// only a guaranteed Fail can carry the fatal error past it.
	end := time.After(10 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-sub.Events():
		case <-end:
			t.Fatal("the corrupted stream did not end")
		}
	}
	var errs []error
	for err := range sub.Errors() {
		errs = append(errs, err)
	}
	if !hasError(errs, store.ErrEventsCorrupted) {
		t.Fatalf("errors = %v, want the fatal ErrEventsCorrupted", errs)
	}
}

// T12: a run this process does not produce keeps alert detection: its
// appended run_failed is tailed into the alert manager, whose alert reaches
// the subscriber out of band and whose run_health twin arrives persisted.
func TestLiveEventsExternalAlertDetectionKept(t *testing.T) {
	svc, native, _ := newLiveEventsService(t, WithAlerts(AlertSettings{}))
	if err := seedOriginRun(t.Context(), native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	sub, err := runstream.SubscribeEventsCursor(t.Context(), svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if !tailerAttached(svc, "r") {
		t.Fatal("no events tailer feeds alert detection")
	}
	if _, err := native.AppendEvent(t.Context(), "r", store.Event{Type: store.EventRunFailed, Data: map[string]any{"error": "boom"}}); err != nil {
		t.Fatal(err)
	}
	got := awaitEvents(t, sub, 10*time.Second, func(evs []*store.Event) bool {
		var alert, health bool
		for _, ev := range evs {
			alert = alert || ev.Type == store.EventAlert
			health = health || ev.Type == store.EventRunHealth && ev.Seq > 0
		}
		return alert && health
	})
	for _, ev := range got {
		if ev.Type == store.EventAlert && ev.Seq != 0 {
			t.Fatalf("alert carries seq %d", ev.Seq)
		}
	}
}

// T13 and F8: an alert still buffered on the broker lane when the retained
// subscription ends cleanly is delivered before the end.
func TestLiveEventsBufferedAlertFlushedAtTheEnd(t *testing.T) {
	prev := svcEventTerminalPollInterval
	svcEventTerminalPollInterval = time.Hour
	t.Cleanup(func() { svcEventTerminalPollInterval = prev })
	svc, native, _ := newLiveEventsService(t)
	if err := seedOriginRun(t.Context(), native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	// The hold: the retained subscription has ended; the alert is published
	// now, so only the end-of-stream flush can deliver it.
	svc.liveEvents.beforeFinish = func(context.Context) {
		svc.broker.Publish(store.Event{RunID: "r", Type: store.EventAlert, NodeID: "late-alert"})
	}
	sub, err := runstream.SubscribeEventsCursor(t.Context(), svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if _, err := native.AppendEvent(t.Context(), "r", store.Event{Type: store.EventRunFinished}); err != nil {
		t.Fatal(err)
	}
	if err := native.UpdateRunStatus(t.Context(), "r", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	svc.broker.CloseRun("r")
	got, errs := drainEventsWithin(t, sub, 5*time.Second)
	if len(errs) != 0 {
		t.Fatalf("errors = %v, want a clean end", errs)
	}
	if len(got) == 0 || got[len(got)-1].Type != store.EventAlert || got[len(got)-1].NodeID != "late-alert" {
		t.Fatalf("delivered %+v, want the buffered alert last", got)
	}
}

// T14: a paused run's stream stays open and keeps delivering.
func TestLiveEventsPausedStaysOpen(t *testing.T) {
	prev := svcEventTerminalPollInterval
	svcEventTerminalPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { svcEventTerminalPollInterval = prev })
	svc, _ := newStreamTestService(t)
	seedStreamRun(t, svc, "r", 1)
	sub, err := svc.StreamSource().SubscribeEvents(t.Context(), "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if err := svc.store.UpdateRunStatus(t.Context(), "r", store.RunStatusPausedOperator, ""); err != nil {
		t.Fatal(err)
	}
	expectNoEvent(t, sub, 500*time.Millisecond, "paused run")
	if _, err := svc.store.AppendEvent(t.Context(), "r", store.Event{Type: store.EventRunResumed}); err != nil {
		t.Fatal(err)
	}
	got := awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
	if got[0].Seq != 1 {
		t.Fatalf("delivered %v, want seq 1", seqsOf(got))
	}
}

// T15: CloseRun on a run still running on disk keeps the stream and its alert
// lane: a later alert and a later append are both delivered.
func TestLiveEventsCloseRunWhileRunningKeepsTheLane(t *testing.T) {
	svc, native, _ := newLiveEventsService(t)
	if err := seedOriginRun(t.Context(), native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	sub, err := runstream.SubscribeEventsCursor(t.Context(), svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	svc.broker.CloseRun("r")
	deadline := time.Now().Add(5 * time.Second)
	for svc.broker.SubscriberCount("r") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the alert lane was not re-subscribed after CloseRun")
		}
		time.Sleep(5 * time.Millisecond)
	}
	svc.broker.Publish(store.Event{RunID: "r", Type: store.EventAlert, NodeID: "after-close"})
	got := awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
	if got[0].Type != store.EventAlert {
		t.Fatalf("delivered %+v, want the alert", got)
	}
	if _, err := native.AppendEvent(t.Context(), "r", store.Event{Type: store.EventNodeFinished}); err != nil {
		t.Fatal(err)
	}
	got = awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
	if got[0].Seq != 1 {
		t.Fatalf("delivered %v, want seq 1", seqsOf(got))
	}
}

// T17: the native filesystem store takes the retained path; a wrapper of it
// keeps the legacy one.
func TestLiveEventsPathGuard(t *testing.T) {
	svc, _ := newStreamTestService(t)
	if svc.liveEvents == nil {
		t.Fatal("native store without live event streams")
	}
	seedStreamRun(t, svc, "r", 1)
	sub, err := svc.StreamSource().SubscribeEvents(t.Context(), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if n := liveRegistered(svc); n != 1 {
		t.Fatalf("registered streams = %d, want the subscription", n)
	}
	wrapped, _, _ := newOriginStreamService(t)
	if wrapped.liveEvents != nil {
		t.Fatal("a wrapped store took the native path")
	}
}

// A publication on the run ID wakes a drain of the retained selection at once,
// without waiting for a terminal poll or the watcher: after the predecessor's
// directory moved away its watch sees no append, so only the wake can surface
// the refusal before the defensive poll.
func TestLiveEventsPublicationWakesTheRetainedDrain(t *testing.T) {
	prev := svcEventTerminalPollInterval
	svcEventTerminalPollInterval = time.Hour
	t.Cleanup(func() { svcEventTerminalPollInterval = prev })
	svc, native, dir := newLiveEventsService(t)
	if err := seedOriginRun(t.Context(), native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	bound := boundOriginCtx(t, svc, "r")
	sub, err := runstream.SubscribeEventsCursor(bound, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	reachLive(t, svc, native, sub, 1)
	// The predecessor's own append can leave a notification queued; let it
	// drain the still-valid run before the move, so none fires after it.
	time.Sleep(300 * time.Millisecond)
	moveRunAside(t, dir, "r")
	if err := seedOriginRun(t.Context(), native, "r", "B", store.LegacyValueFormat, store.RunStatusRunning, 3); err != nil {
		t.Fatal(err)
	}
	successor, err := native.LoadEvents(t.Context(), "r")
	if err != nil {
		t.Fatal(err)
	}
	svc.broker.Publish(*successor[2])
	got, errs := drainEventsWithin(t, sub, 3*time.Second)
	if len(got) != 0 || !hasError(errs, store.ErrRunDataIdentityChanged) {
		t.Fatalf("delivered %+v, errors %v: want the identity refusal alone", got, errs)
	}
}

// A real in-process subbot tree, drained by the production entry point. The
// child run never gets CloseRun (only its root does): its stream still ends
// clean, after what Drain persisted for it, not cut with ErrSourceClosed.
func TestLiveEventsDrainEndsASubbotChildStreamClean(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "control_child.bot"), []byte(subbotControlChild), 0o644); err != nil {
		t.Fatal(err)
	}
	parentPath := filepath.Join(dir, "control_parent.bot")
	if err := os.WriteFile(parentPath, []byte(subbotControlParent), 0o644); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopService(t, svc) })
	if svc.liveEvents == nil {
		t.Fatal("native store without live event streams")
	}
	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: parentPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { awaitRunCompletion(t, res.Done, "the drained parent outlived the test") })
	childID := waitForActiveChild(t, svc, res.RunID)
	child, err := svc.StreamSource().SubscribeEvents(t.Context(), childID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	root, err := svc.StreamSource().SubscribeEvents(t.Context(), res.RunID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// Both streams are past their first terminal check.
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc.Drain(ctx)
	got, errs := drainEventsWithin(t, child, 10*time.Second)
	if len(errs) != 0 {
		t.Fatalf("drained child's stream errors = %v, want its own clean end", errs)
	}
	ended := false
	for _, ev := range got {
		ended = ended || ev.Type == store.EventRunCancelled || ev.Type == store.EventRunInterrupted
	}
	if !ended {
		t.Fatalf("drained child's stream delivered %v, want its run_cancelled or run_interrupted", typesOfEvents(got))
	}
	if _, rootErrs := drainEventsWithin(t, root, 10*time.Second); len(rootErrs) != 0 {
		t.Fatalf("drained root's stream errors = %v, want its own clean end", rootErrs)
	}
}

func typesOfEvents(evs []*store.Event) []store.EventType {
	out := make([]store.EventType, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}

// The detached runner's order: its exit watcher sends CloseRun while the
// status is still running, then the handle ends, and only then does Drain
// persist the interruption. The stream still ends clean within the grace,
// with that interruption.
func TestLiveEventsDrainEndsADetachedRunStreamClean(t *testing.T) {
	svc, native, _ := newLiveEventsService(t)
	if err := seedOriginRun(t.Context(), native, "d", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	finish := sync.OnceFunc(func() { close(done) })
	t.Cleanup(finish)
	var once sync.Once
	exit := func() {
		once.Do(func() {
			go func() {
				svc.broker.CloseRun("d")
				// The stream checks the status on that CloseRun while it is
				// still running, before the handle ends.
				time.Sleep(200 * time.Millisecond)
				finish()
			}()
		})
	}
	if err := svc.manager.RegisterDetached("d", 0, exit, done); err != nil {
		t.Fatal(err)
	}
	bound := boundOriginCtx(t, svc, "d")
	sub, err := runstream.SubscribeEventsCursor(bound, svc.StreamSource(), "d", runstream.EventCursor{Seq: 0, After: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	// The stream is past its first terminal check.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	svc.Drain(context.Background())
	took := time.Since(start)
	got, errs := drainEventsWithin(t, sub, 5*time.Second)
	if len(errs) != 0 {
		t.Fatalf("drained detached run's stream errors = %v after a %v Drain, want its own clean end", errs, took)
	}
	interrupted := false
	for _, ev := range got {
		interrupted = interrupted || ev.Type == store.EventRunInterrupted
	}
	if !interrupted {
		t.Fatalf("drained detached run's stream delivered %v, want its run_interrupted", typesOfEvents(got))
	}
}

// drainReturnsWithin runs Drain and fails unless it returns within limit.
func drainReturnsWithin(t *testing.T, svc *Service, limit time.Duration) time.Duration {
	t.Helper()
	took := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		svc.Drain(context.Background())
		took <- time.Since(start)
	}()
	select {
	case d := <-took:
		return d
	case <-time.After(limit):
		t.Fatalf("Drain did not return within %v", limit)
		return 0
	}
}

// A drained run whose stream never ends on its own (it ends paused, which is
// not terminal) holds Drain no longer than the grace.
func TestLiveEventsDrainGraceBoundsAStreamThatDoesNotEnd(t *testing.T) {
	svc, native, _ := newLiveEventsService(t)
	if err := seedOriginRun(t.Context(), native, "p", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	runCtx, err := svc.manager.Register(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-runCtx.Done()
		_ = native.UpdateRunStatus(context.Background(), "p", store.RunStatusPausedOperator, "")
		svc.manager.Deregister("p")
	}()
	sub, err := svc.StreamSource().SubscribeEvents(t.Context(), "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
	// Drain starts first: the grace is paid while Drain runs, not before it.
	// The grace is what Drain pays for a run that will not end on its own:
	// the paused stream stays open for all of it, then Drain returns.
	drained := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		svc.Drain(context.Background())
		drained <- time.Since(start)
	}()
	select {
	case batch, ok := <-sub.Events():
		t.Fatalf("the paused drained stream ended during the grace: %v (open %v)", batch, ok)
	case <-time.After(liveEventsDrainGrace - 200*time.Millisecond):
	}
	select {
	case d := <-drained:
		if d > liveEventsDrainGrace+1*time.Second {
			t.Fatalf("Drain took %v, want within the grace + 1 s", d)
		}
	case <-time.After(liveEventsDrainGrace + 3*time.Second):
		t.Fatalf("Drain did not return within grace + 3 s")
	}
}

// A stream of a run Drain did not drain costs Drain nothing once the drained
// runs' streams ended.
func TestLiveEventsDrainDoesNotWaitForStreamsOfOtherRuns(t *testing.T) {
	svc, native, _ := newLiveEventsService(t)
	for _, id := range []string{"r", "other"} {
		if err := seedOriginRun(t.Context(), native, id, "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, err := svc.manager.Register(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-runCtx.Done()
		_ = native.UpdateRunStatus(context.Background(), "r", store.RunStatusCancelled, "")
		if ev, err := native.AppendEvent(context.Background(), "r", store.Event{Type: store.EventRunCancelled}); err == nil {
			svc.broker.Publish(*ev)
		}
		svc.broker.CloseRun("r")
		svc.manager.Deregister("r")
	}()
	other, err := svc.StreamSource().SubscribeEvents(t.Context(), "other", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if d := drainReturnsWithin(t, svc, 6*time.Second); d > time.Second {
		t.Fatalf("Drain took %v waiting for a stream of a run it did not drain", d)
	}
}

// Drain asks only the drained runs' streams for their final status. A stream
// of a run Drain did not drain (another process's run) whose writer has
// persisted its terminal status but not yet its terminal event must not be
// told its events are over: it ends with ErrSourceClosed, or with its
// terminal event, never with a clean end that lacks it.
func TestLiveEventsDrainNeverEndsAnotherRunsStreamAtItsBarrier(t *testing.T) {
	svc, native, _ := newLiveEventsService(t)
	for _, id := range []string{"r", "ext"} {
		if err := seedOriginRun(t.Context(), native, id, "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
			t.Fatal(err)
		}
	}
	// The drained run's stream lingers 1 s before its end, so Drain's grace
	// wait is long enough for any hint it sent elsewhere to act.
	svc.liveEvents.beforeFinish = func(ctx context.Context) {
		if ctx.Value(rva3fSlowKey{}) != nil {
			time.Sleep(time.Second)
		}
	}
	runCtx, err := svc.manager.Register(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-runCtx.Done()
		_ = native.UpdateRunStatus(context.Background(), "r", store.RunStatusCancelled, "")
		if ev, err := native.AppendEvent(context.Background(), "r", store.Event{Type: store.EventRunCancelled}); err == nil {
			svc.broker.Publish(*ev)
		}
		svc.broker.CloseRun("r")
		svc.manager.Deregister("r")
	}()
	slow := context.WithValue(t.Context(), rva3fSlowKey{}, true)
	rs, err := svc.StreamSource().SubscribeEvents(slow, "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	ext, err := svc.StreamSource().SubscribeEvents(t.Context(), "ext", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ext.Close()
	awaitEvents(t, ext, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
	awaitEvents(t, rs, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
	// Both streams are past their first terminal check (poll 5 s).
	time.Sleep(150 * time.Millisecond)
	// Another process's status-first terminal writer: status now, event later.
	if err := native.UpdateRunStatus(t.Context(), "ext", store.RunStatusFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	svc.Drain(context.Background())
	got, errs := drainEventsWithin(t, ext, 5*time.Second)
	terminal := false
	for _, ev := range got {
		terminal = terminal || ev.Type == store.EventRunFailed
	}
	t.Logf("ext stream delivered %v, errors %v", typesOfEvents(got), errs)
	if len(errs) == 0 && !terminal {
		t.Fatalf("a stream of a run Drain did not drain ended clean without its terminal event (errors %v)", errs)
	}
	if !hasError(errs, runstream.ErrSourceClosed) && !terminal {
		t.Fatalf("ext stream errors = %v, want ErrSourceClosed", errs)
	}
}

// Two streams of one drained run in the detached order (CloseRun while still
// running, the handle ends, then Drain persists the interruption): each stream
// is asked for the final status, so both end clean with it.
func TestLiveEventsDrainEndsEveryStreamOfADrainedRun(t *testing.T) {
	svc, native, _ := newLiveEventsService(t)
	if err := seedOriginRun(t.Context(), native, "d", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	finish := sync.OnceFunc(func() { close(done) })
	t.Cleanup(finish)
	var once sync.Once
	exit := func() {
		once.Do(func() {
			go func() {
				svc.broker.CloseRun("d")
				time.Sleep(200 * time.Millisecond)
				finish()
			}()
		})
	}
	if err := svc.manager.RegisterDetached("d", 0, exit, done); err != nil {
		t.Fatal(err)
	}
	var subs []runstream.EventSubscription
	for i := 0; i < 2; i++ {
		bound := boundOriginCtx(t, svc, "d")
		sub, err := runstream.SubscribeEventsCursor(bound, svc.StreamSource(), "d", runstream.EventCursor{Seq: 0, After: true})
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
		subs = append(subs, sub)
	}
	time.Sleep(100 * time.Millisecond)
	svc.Drain(context.Background())
	for i, sub := range subs {
		got, errs := drainEventsWithin(t, sub, 5*time.Second)
		interrupted := false
		for _, ev := range got {
			interrupted = interrupted || ev.Type == store.EventRunInterrupted
		}
		if len(errs) != 0 || !interrupted {
			t.Fatalf("stream %d of the drained run: errors %v, delivered %v; want its own clean end with run_interrupted", i, errs, typesOfEvents(got))
		}
	}
}

// The deadline path: the run outlives Drain's context; Drain marks it
// interrupted while its engine still runs. A clean end would claim the run's
// events are over while its engine still writes.
func TestLiveEventsDrainDeadlinePathStreamEnd(t *testing.T) {
	svc, native, _ := newLiveEventsService(t)
	if err := seedOriginRun(t.Context(), native, "x", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(sync.OnceFunc(func() { close(done) }))
	if err := svc.manager.RegisterDetached("x", 0, func() {}, done); err != nil {
		t.Fatal(err)
	}
	sub, err := svc.StreamSource().SubscribeEvents(t.Context(), "x", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	awaitEvents(t, sub, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	svc.Drain(ctx)
	got, errs := drainEventsWithin(t, sub, 5*time.Second)
	t.Logf("deadline-path stream end: delivered %v, errors %v", typesOfEvents(got), errs)
	if len(errs) == 0 {
		t.Fatalf("the stream of a run still running at Drain's deadline ended clean (delivered %v)", typesOfEvents(got))
	}
}

type rva3fSlowKey struct{}

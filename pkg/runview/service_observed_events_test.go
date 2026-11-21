package runview

import (
	"bytes"
	"context"
	"errors"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func serviceFeedFixture(t *testing.T, opts ...ServiceOption) (*Service, *store.FilesystemRunStore) {
	t.Helper()
	all := append([]ServiceOption{WithRecoveryPassive()}, opts...)
	s, e := NewService(t.TempDir(), all...)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c, cancel := context.WithTimeout(context.Background(), time.Second); defer cancel(); s.Stop(c) })
	st, _ := s.store.(*store.FilesystemRunStore)
	return s, st
}
func serviceFeedOrigin(t *testing.T, st *store.FilesystemRunStore) store.RunObservation {
	t.Helper()
	if _, e := st.CreateRun(t.Context(), "r", "wf", nil); e != nil {
		t.Fatal(e)
	}
	ev, e := st.AppendEvent(t.Context(), "r", store.Event{})
	if e != nil {
		t.Fatal(e)
	}
	o, k := ev.EventObservation()
	if !k {
		t.Fatal("missing native origin")
	}
	return o
}
func serviceFeedReceive(t *testing.T, h ObservedEventFeed, o store.RunObservation) {
	t.Helper()
	select {
	case e, ok := <-h.Events():
		if !ok || e == nil {
			t.Fatal("no event", h.Err())
		}
		got, k := e.EventObservation()
		if !k || got != o {
			t.Fatal("wrong original")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no positive delivery")
	}
}
func serviceFeedAcquire(t *testing.T, s *Service, o store.RunObservation) ObservedEventFeed {
	t.Helper()
	h, e := s.AcquireObservedEventFeed(t.Context(), o)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { h.Close() })
	return h
}
func TestServiceObservedFeedNativeOwnership(t *testing.T) {
	s, st := serviceFeedFixture(t)
	o := serviceFeedOrigin(t, st)
	a := serviceFeedAcquire(t, s, o)
	serviceFeedReceive(t, a, o)
	b := serviceFeedAcquire(t, s, o)
	a.Close()
	select {
	case <-a.Done():
		t.Fatal("non-last Close stopped shared producer")
	default:
	}
	if _, e := st.AppendEvent(t.Context(), "r", store.Event{}); e != nil {
		t.Fatal(e)
	}
	serviceFeedReceive(t, b, o)
	s.StopBackground(t.Context())
	select {
	case <-b.Done():
	default:
		t.Fatal("StopBackground returned before producer joined")
	}
	if b.Err() == nil {
		t.Fatal("shutdown cause absent")
	}
	r, e := st.LoadRun(t.Context(), "r")
	if e != nil || r.Status != store.RunStatusRunning {
		t.Fatal("StopBackground changed status", r, e)
	}
	if _, e = st.AppendEvent(t.Context(), "r", store.Event{}); e != nil {
		t.Fatal("borrowed store unavailable", e)
	}
	if h, e := s.AcquireObservedEventFeed(t.Context(), o); h != nil || e == nil {
		t.Fatal("admission reopened", e)
	}
	s.StopBackground(t.Context())
}
func TestServiceObservedFeedSeparateServices(t *testing.T) {
	a, as := serviceFeedFixture(t)
	b, bs := serviceFeedFixture(t)
	oa, ob := serviceFeedOrigin(t, as), serviceFeedOrigin(t, bs)
	ha, hb := serviceFeedAcquire(t, a, oa), serviceFeedAcquire(t, b, ob)
	serviceFeedReceive(t, ha, oa)
	serviceFeedReceive(t, hb, ob)
	a.Stop(t.Context())
	select {
	case <-ha.Done():
	default:
		t.Fatal("Stop did not join")
	}
	if _, e := bs.AppendEvent(t.Context(), "r", store.Event{}); e != nil {
		t.Fatal(e)
	}
	serviceFeedReceive(t, hb, ob)
}

type serviceFeedWrappedStore struct{ store.RunStore }

func TestServiceObservedFeedUnsupported(t *testing.T) {
	var closes atomic.Int32
	borrowed := &bridgeProbeSource{close: func() error { closes.Add(1); return nil }}
	s, _ := serviceFeedFixture(t, WithStreamSource(borrowed))
	if s.StreamSource() != borrowed {
		t.Fatal("borrowed Source changed")
	}
	if h, e := s.AcquireObservedEventFeed(t.Context(), bridgeOrigin(t)); h != nil || !errors.Is(e, store.ErrRunDataCapability) {
		t.Fatal(e)
	}
	s.Stop(t.Context())
	if closes.Load() != 0 {
		t.Fatal("borrowed source closed")
	}
	st, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	wrapped, _ := serviceFeedFixture(t, WithStore(serviceFeedWrappedStore{st}))
	if h, e := wrapped.AcquireObservedEventFeed(t.Context(), bridgeOrigin(t)); h != nil || !errors.Is(e, store.ErrRunDataCapability) {
		t.Fatal(e)
	}
	var zero Service
	if h, e := zero.AcquireObservedEventFeed(t.Context(), bridgeOrigin(t)); h != nil || !errors.Is(e, store.ErrRunDataCapability) {
		t.Fatal(e)
	}
}
func TestServiceObservedFeedPendingStopTimeoutRetry(t *testing.T) {
	s, _ := serviceFeedFixture(t)
	o := bridgeOrigin(t)
	sub := bridgeSub()
	entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	var closes atomic.Int32
	s.observedEvents.open = func(context.Context) (runstream.Source, error) {
		return &bridgeProbeSource{subscribe: func(c context.Context) (runstream.EventSubscription, error) {
			close(entered)
			<-c.Done()
			close(canceled)
			<-release
			return sub, nil
		}, close: func() error { closes.Add(1); return nil }}, nil
	}
	result := make(chan error, 1)
	go func() {
		h, e := s.AcquireObservedEventFeed(t.Context(), o)
		if h != nil {
			h.Close()
		}
		result <- e
	}()
	bridgeWait(t, entered)
	// Cleanup release must precede the fixture's Stop, even on assertion failure.
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	expired, cancel := context.WithCancel(t.Context())
	cancel()
	var logs bytes.Buffer
	s.logger = iterlog.New(iterlog.LevelInfo, &logs)
	s.StopBackground(expired)
	bridgeWait(t, canceled)
	if closes.Load() != 0 {
		t.Fatal("premature cleanup")
	}
	if !bytes.Contains(logs.Bytes(), []byte("shutdown cleanup not joined")) {
		t.Fatal("timeout was not reported", logs.String())
	}
	s.observedEvents.mu.Lock()
	entry := s.observedEvents.entries[o]
	closed := s.observedEvents.closed
	s.observedEvents.mu.Unlock()
	if entry == nil || !closed {
		t.Fatal("pending ownership discarded")
	}
	select {
	case <-entry.done:
		t.Fatal("pending work falsely joined")
	default:
	}
	if h, e := s.AcquireObservedEventFeed(t.Context(), o); h != nil || e == nil {
		t.Fatal("new admission after timeout", e)
	}
	once.Do(func() { close(release) })
	if e := s.observedEvents.stop(t.Context()); e != nil {
		t.Fatal(e)
	}
	bridgeWait(t, entry.done)
	select {
	case e := <-result:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("pending acquire not released")
	}
	if closes.Load() != 1 || sub.closes.Load() != 1 {
		t.Fatal("late resource cleanup not exact")
	}
	if e := s.observedEvents.stop(expired); e != nil {
		t.Fatal("already joined stop should succeed", e)
	}
}
func TestServiceObservedFeedStopWaitsOwnedCleanup(t *testing.T) {
	s, _ := serviceFeedFixture(t)
	o := bridgeOrigin(t)
	sub := bridgeSub()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.observedEvents.open = func(context.Context) (runstream.Source, error) {
		return &bridgeProbeSource{subscribe: func(context.Context) (runstream.EventSubscription, error) { return sub, nil }, close: func() error { close(entered); <-release; return nil }}, nil
	}
	h := serviceFeedAcquire(t, s, o)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	stopped := make(chan struct{})
	go func() { s.StopBackground(t.Context()); close(stopped) }()
	bridgeWait(t, entered)
	select {
	case <-stopped:
		t.Fatal("Stop returned before owned source Close")
	case <-time.After(25 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	bridgeWait(t, stopped)
	select {
	case <-h.Done():
	default:
		t.Fatal("owned Done not joined")
	}
}
func TestServiceObservedFeedRetiringWaiterCannotRestart(t *testing.T) {
	s, _ := serviceFeedFixture(t)
	o := bridgeOrigin(t)
	sub := bridgeSub()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var opened atomic.Int32
	s.observedEvents.open = func(context.Context) (runstream.Source, error) {
		opened.Add(1)
		return &bridgeProbeSource{subscribe: func(context.Context) (runstream.EventSubscription, error) { return sub, nil }, close: func() error { close(entered); <-release; return nil }}, nil
	}
	h := serviceFeedAcquire(t, s, o)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	sub.eof()
	bridgeWait(t, entered)
	result := make(chan error, 1)
	go func() {
		h, e := s.AcquireObservedEventFeed(t.Context(), o)
		if h != nil {
			h.Close()
		}
		result <- e
	}()
	expired, cancel := context.WithCancel(t.Context())
	cancel()
	if e := s.observedEvents.stop(expired); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	once.Do(func() { close(release) })
	bridgeWait(t, h.Done())
	select {
	case e := <-result:
		if e == nil {
			t.Fatal("retiring waiter restarted")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter stuck")
	}
	if opened.Load() != 1 {
		t.Fatal("successor created during Stop")
	}
}

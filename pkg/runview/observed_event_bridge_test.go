package runview

import (
	"context"
	"errors"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type bridgeProbeSub struct {
	events chan []*store.Event
	errs   chan error
	once   sync.Once
	closes atomic.Int32
}

func bridgeSub() *bridgeProbeSub {
	return &bridgeProbeSub{events: make(chan []*store.Event, 8), errs: make(chan error, 8)}
}
func (s *bridgeProbeSub) Events() <-chan []*store.Event { return s.events }
func (s *bridgeProbeSub) Errors() <-chan error          { return s.errs }
func (s *bridgeProbeSub) eof() {
	s.once.Do(func() {
		if s.events != nil {
			close(s.events)
		}
		if s.errs != nil {
			close(s.errs)
		}
	})
}
func (s *bridgeProbeSub) Close() error { s.closes.Add(1); s.eof(); return nil }

type bridgeProbeSource struct {
	subscribe func(context.Context) (runstream.EventSubscription, error)
	close     func() error
}

func (s *bridgeProbeSource) SubscribeEvents(c context.Context, _ string, _ int64) (runstream.EventSubscription, error) {
	return s.subscribe(c)
}
func (s *bridgeProbeSource) SubscribeLogs(context.Context, string, int64) (runstream.LogSubscription, error) {
	return nil, store.ErrRunDataCapability
}
func (s *bridgeProbeSource) Close() error {
	if s.close != nil {
		return s.close()
	}
	return nil
}
func bridgeRegistry(t *testing.T) (*observedEventBridges, *store.FilesystemRunStore) {
	t.Helper()
	s, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	c, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	r, e := newObservedEventBridges(c, &Service{store: s, broker: NewEventBroker(), logger: iterlog.New(iterlog.LevelInfo, io.Discard)})
	if e != nil {
		t.Fatal(e)
	}
	return r, s
}
func bridgeOrigin(t *testing.T) store.RunObservation {
	t.Helper()
	o, e := store.CaptureRunObservation(store.RunDataHeader{RunID: "r", Format: 1, BirthID: "A"}, "", true)
	if e != nil {
		t.Fatal(e)
	}
	return o
}
func bridgeEvent(t *testing.T, o store.RunObservation) *store.Event {
	t.Helper()
	e, err := store.WithEventObservation(store.Event{RunID: "r"}, o)
	if err != nil {
		t.Fatal(err)
	}
	return &e
}
func bridgeAcquire(t *testing.T, r *observedEventBridges, o store.RunObservation) *observedBridgeLease {
	t.Helper()
	h, e := r.acquire(t.Context(), o)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { h.Close() })
	return h
}
func bridgeWait(t *testing.T, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(8 * time.Second):
		t.Fatal("cleanup did not finish")
	}
}
func bridgeReceive(t *testing.T, h *observedBridgeLease, o store.RunObservation) {
	t.Helper()
	select {
	case e, ok := <-h.Events():
		if !ok || e == nil {
			t.Fatal("missing event", h.Err())
		}
		got, known := e.EventObservation()
		if !known || got != o {
			t.Fatal("wrong origin")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("no event")
	}
}
func bridgeClosed(t *testing.T, h *observedBridgeLease) {
	t.Helper()
	select {
	case _, ok := <-h.Events():
		if ok {
			t.Fatal("unexpected delivery")
		}
	case <-time.After(time.Second):
		t.Fatal("old subscription still open")
	}
}
func TestObservedBridgeNative(t *testing.T) {
	r, s := bridgeRegistry(t)
	c := t.Context()
	if _, e := s.CreateRun(c, "r", "wf", nil); e != nil {
		t.Fatal(e)
	}
	a, e := s.AppendEvent(c, "r", store.Event{})
	if e != nil {
		t.Fatal(e)
	}
	o, k := a.EventObservation()
	if !k {
		t.Fatal("native origin absent")
	}
	h := bridgeAcquire(t, r, o)
	bridgeReceive(t, h, o)
	if _, e = s.AppendEvent(c, "r", store.Event{}); e != nil {
		t.Fatal(e)
	}
	bridgeReceive(t, h, o)
	if e = s.DeleteRun(c, "r"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.PruneDeletionMarkers(c, time.Now().Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRun(c, "r", "wf", nil); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AppendEvent(c, "r", store.Event{}); e != nil {
		t.Fatal(e)
	}
	bridgeWait(t, h.Done())
	if !runstream.IsPermanentError(h.Err()) {
		t.Fatal(h.Err())
	}
	bridgeClosed(t, h)
}
func TestObservedBridgeRetiringBarrier(t *testing.T) {
	r, _ := bridgeRegistry(t)
	o := bridgeOrigin(t)
	s1, s2 := bridgeSub(), bridgeSub()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var count atomic.Int32
	r.open = func(context.Context) (runstream.Source, error) {
		n := count.Add(1)
		s := s1
		if n > 1 {
			s = s2
		}
		return &bridgeProbeSource{subscribe: func(context.Context) (runstream.EventSubscription, error) { return s, nil }, close: func() error {
			r.mu.Lock()
			current := r.entries[o]
			retiring := current != nil && current.phase == bridgeRetiring
			r.mu.Unlock()
			if !retiring {
				t.Error("source cleanup lost its retiring registry entry")
			}
			if n == 1 {
				close(entered)
				<-release
			}
			return nil
		}}, nil
	}
	h1 := bridgeAcquire(t, r, o)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	s1.eof()
	bridgeWait(t, entered)
	next := make(chan *observedBridgeLease, 1)
	fail := make(chan error, 1)
	go func() {
		h, e := r.acquire(t.Context(), o)
		if e != nil {
			fail <- e
			return
		}
		next <- h
	}()
	select {
	case <-next:
		t.Fatal("successor escaped barrier")
	case <-time.After(30 * time.Millisecond):
	}
	if count.Load() != 1 {
		t.Fatal("new source before old cleanup")
	}
	bridgeClosed(t, h1)
	once.Do(func() { close(release) })
	var h2 *observedBridgeLease
	select {
	case h2 = <-next:
	case e := <-fail:
		t.Fatal(e)
	case <-time.After(time.Second):
		t.Fatal("successor stuck")
	}
	t.Cleanup(func() { h2.Close() })
	s2.events <- []*store.Event{bridgeEvent(t, o)}
	bridgeReceive(t, h2, o)
	h1.Close()
	s2.events <- []*store.Event{bridgeEvent(t, o)}
	bridgeReceive(t, h2, o)
}
func TestObservedBridgePendingCancel(t *testing.T) {
	r, _ := bridgeRegistry(t)
	o := bridgeOrigin(t)
	s1, s2 := bridgeSub(), bridgeSub()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var count, closed atomic.Int32
	r.open = func(context.Context) (runstream.Source, error) {
		n := count.Add(1)
		return &bridgeProbeSource{subscribe: func(context.Context) (runstream.EventSubscription, error) {
			if n == 1 {
				close(entered)
				<-release
				return s1, nil
			}
			return s2, nil
		}, close: func() error { closed.Add(1); return nil }}, nil
	}
	c, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		h, e := r.acquire(c, o)
		if h != nil {
			h.Close()
		}
		result <- e
	}()
	bridgeWait(t, entered)
	cancel()
	deadline := time.After(time.Second)
	for {
		r.mu.Lock()
		e := r.entries[o]
		retiring := e != nil && e.phase == bridgeRetiring && e.refs == 0
		r.mu.Unlock()
		if retiring {
			break
		}
		select {
		case <-deadline:
			t.Fatal("pending reference not retired")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	next := make(chan *observedBridgeLease, 1)
	errs := make(chan error, 1)
	go func() {
		h, e := r.acquire(t.Context(), o)
		if e != nil {
			errs <- e
			return
		}
		next <- h
	}()
	select {
	case <-next:
		t.Fatal("pending successor escaped")
	case <-time.After(30 * time.Millisecond):
	}
	if count.Load() != 1 {
		t.Fatal("early source")
	}
	once.Do(func() { close(release) })
	select {
	case e := <-result:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel stuck")
	}
	var h *observedBridgeLease
	select {
	case h = <-next:
	case e := <-errs:
		t.Fatal(e)
	case <-time.After(time.Second):
		t.Fatal("successor stuck")
	}
	t.Cleanup(func() { h.Close() })
	if s1.closes.Load() != 1 || closed.Load() != 1 {
		t.Fatal("late cleanup not exact")
	}
	s2.events <- []*store.Event{bridgeEvent(t, o)}
	bridgeReceive(t, h, o)
}
func TestObservedBridgeCopiesAndJoinAdmission(t *testing.T) {
	r, _ := bridgeRegistry(t)
	o := bridgeOrigin(t)
	s := bridgeSub()
	var opened atomic.Int32
	r.open = func(context.Context) (runstream.Source, error) {
		opened.Add(1)
		return &bridgeProbeSource{subscribe: func(context.Context) (runstream.EventSubscription, error) { return s, nil }}, nil
	}
	a, b := bridgeAcquire(t, r, o), bridgeAcquire(t, r, o)
	copyA := *a
	copyA.Close()
	a.Close()
	if s.closes.Load() != 0 {
		t.Fatal("copied handle prematurely closed the source while H2 remains")
	}
	s.events <- []*store.Event{bridgeEvent(t, o)}
	bridgeReceive(t, b, o)
	if opened.Load() != 1 {
		t.Fatal("duplicate producer")
	}
	if h, e := r.acquire(store.WithOwner(t.Context(), "other"), o); h != nil || !errors.Is(e, store.ErrRunDataIdentityChanged) {
		t.Fatal(e)
	}
	b.Close()
	bridgeWait(t, b.Done())
	if s.closes.Load() != 1 {
		t.Fatal("cleanup count")
	}
}
func TestObservedBridgeBatchAndTerminal(t *testing.T) {
	for _, mode := range []string{"unknown", "mismatch", "queued", "warning"} {
		t.Run(mode, func(t *testing.T) {
			r, _ := bridgeRegistry(t)
			o := bridgeOrigin(t)
			s := bridgeSub()
			r.open = func(context.Context) (runstream.Source, error) {
				return &bridgeProbeSource{subscribe: func(context.Context) (runstream.EventSubscription, error) { return s, nil }}, nil
			}
			h := bridgeAcquire(t, r, o)
			switch mode {
			case "unknown":
				s.events <- []*store.Event{bridgeEvent(t, o), &store.Event{RunID: "r"}}
			case "mismatch":
				b, e := store.CaptureRunObservation(store.RunDataHeader{RunID: "r", Format: 1, BirthID: "B"}, "", true)
				if e != nil {
					t.Fatal(e)
				}
				s.events <- []*store.Event{bridgeEvent(t, o), bridgeEvent(t, b)}
			case "queued":
				s.errs <- store.ErrRunDataIdentityChanged
				s.eof()
			case "warning":
				s.errs <- errors.New("warning")
				s.events <- []*store.Event{bridgeEvent(t, o)}
				bridgeReceive(t, h, o)
				s.eof()
			}
			bridgeWait(t, h.Done())
			if mode == "warning" {
				if h.Err() != nil {
					t.Fatal(h.Err())
				}
			} else {
				if !runstream.IsPermanentError(h.Err()) {
					t.Fatal("fatal lost", h.Err())
				}
				bridgeClosed(t, h)
			}
		})
	}
}
func TestObservedBridgeStartupFailures(t *testing.T) {
	for _, mode := range []string{"nil-source", "typednil-source", "partial-source", "nil-sub", "partial-sub", "nil-errors", "nil-events"} {
		t.Run(mode, func(t *testing.T) {
			r, _ := bridgeRegistry(t)
			o := bridgeOrigin(t)
			s := bridgeSub()
			var closed atomic.Int32
			sentinel := errors.New("startup failure")
			if mode == "nil-errors" {
				s.errs = nil
			}
			if mode == "nil-events" {
				s.events = nil
			}
			r.open = func(context.Context) (runstream.Source, error) {
				if mode == "nil-source" {
					return nil, nil
				}
				if mode == "typednil-source" {
					var p *bridgeProbeSource
					return p, nil
				}
				src := &bridgeProbeSource{subscribe: func(context.Context) (runstream.EventSubscription, error) {
					if mode == "nil-sub" {
						return nil, nil
					}
					if mode == "partial-sub" {
						return s, sentinel
					}
					return s, nil
				}, close: func() error { closed.Add(1); return nil }}
				if mode == "partial-source" {
					return src, sentinel
				}
				return src, nil
			}
			result := make(chan error, 1)
			go func() {
				h, e := r.acquire(t.Context(), o)
				if h != nil {
					h.Close()
				}
				result <- e
			}()
			select {
			case e := <-result:
				if e == nil {
					t.Fatal("accepted invalid resource")
				}
			case <-time.After(time.Second):
				t.Fatal("ready/cleanup stuck")
			}
			if mode != "nil-source" && mode != "typednil-source" && closed.Load() != 1 {
				t.Fatal("partial source leaked")
			}
			if (mode == "partial-sub" || mode == "nil-errors" || mode == "nil-events") && s.closes.Load() != 1 {
				t.Fatal("partial sub leaked")
			}
		})
	}
}
func TestObservedBridgeOwnerCause(t *testing.T) {
	r, _ := bridgeRegistry(t)
	o := bridgeOrigin(t)
	s := bridgeSub()
	c, cancel := context.WithCancelCause(t.Context())
	r.lifetime = c
	r.open = func(context.Context) (runstream.Source, error) {
		return &bridgeProbeSource{subscribe: func(context.Context) (runstream.EventSubscription, error) { return s, nil }}, nil
	}
	h := bridgeAcquire(t, r, o)
	cancel(store.ErrRunDataIdentityChanged)
	bridgeWait(t, h.Done())
	if !errors.Is(h.Err(), store.ErrRunDataIdentityChanged) {
		t.Fatal("owner cause lost", h.Err())
	}
}

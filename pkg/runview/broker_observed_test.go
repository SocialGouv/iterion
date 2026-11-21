package runview

import (
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"sync"
	"testing"
	"time"
)

func brokerOrigin(t *testing.T, tenant, birth string) store.RunObservation {
	t.Helper()
	o, e := store.CaptureRunObservation(store.RunDataHeader{RunID: "r", Format: 1, BirthID: birth}, tenant, true)
	if e != nil {
		t.Fatal(e)
	}
	return o
}
func brokerStamped(t *testing.T, o store.RunObservation) store.Event {
	t.Helper()
	e, err := store.WithEventObservation(store.Event{RunID: "r"}, o)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func brokerScope(t *testing.T, b *EventBroker, o store.RunObservation) *ObservedEventScope {
	t.Helper()
	h, e := b.AcquireObserved(o)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { h.Close() })
	return h
}
func brokerSub(t *testing.T, h *ObservedEventScope) *EventSubscription {
	t.Helper()
	s, e := h.Subscribe()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Cancel)
	return s
}
func brokerReceive(t *testing.T, s *EventSubscription, o store.RunObservation) {
	t.Helper()
	select {
	case e, ok := <-s.C:
		if !ok || e == nil {
			t.Fatal("no matching event")
		}
		got, known := e.EventObservation()
		if !known || got != o {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("matching publication absent")
	}
}
func brokerEmpty(t *testing.T, s *EventSubscription) {
	t.Helper()
	select {
	case e := <-s.C:
		t.Fatal("unexpected delivery/close", e)
	default:
	}
}
func TestObservedBrokerNativeDelayedBirth(t *testing.T) {
	s, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	c := t.Context()
	if _, e = s.CreateRun(c, "r", "wf", nil); e != nil {
		t.Fatal(e)
	}
	a, e := s.AppendEvent(c, "r", store.Event{})
	if e != nil {
		t.Fatal(e)
	}
	oa, known := a.EventObservation()
	if !known {
		t.Fatal("native origin missing")
	}
	b := NewEventBroker()
	ha := brokerScope(t, b, oa)
	sa := brokerSub(t, ha)
	if e = s.DeleteRun(c, "r"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.PruneDeletionMarkers(c, time.Now().Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRun(c, "r", "wf", nil); e != nil {
		t.Fatal(e)
	}
	next, e := s.AppendEvent(c, "r", store.Event{})
	if e != nil {
		t.Fatal(e)
	}
	ob, known := next.EventObservation()
	if !known || ob == oa {
		t.Fatal("replacement not distinct", ob)
	}
	hb := brokerScope(t, b, ob)
	sb := brokerSub(t, hb)
	if e = ha.Publish(*a); e != nil {
		t.Fatal(e)
	}
	brokerReceive(t, sa, oa)
	brokerEmpty(t, sb)
	if e = hb.Publish(*next); e != nil {
		t.Fatal(e)
	}
	brokerReceive(t, sb, ob)
	brokerEmpty(t, sa)
}
func TestObservedBrokerSeparationAndLegacy(t *testing.T) {
	b := NewEventBroker()
	a := brokerOrigin(t, "a", "same")
	other := brokerOrigin(t, "b", "same")
	ha := brokerScope(t, b, a)
	hb := brokerScope(t, b, other)
	hc := brokerScope(t, NewEventBroker(), a)
	sa, sb, sc := brokerSub(t, ha), brokerSub(t, hb), brokerSub(t, hc)
	legacy := b.Subscribe("r")
	defer legacy.Cancel()
	event := brokerStamped(t, a)
	b.Publish(event)
	select {
	case <-legacy.C:
	default:
		t.Fatal("legacy publication lost")
	}
	brokerEmpty(t, sa)
	brokerEmpty(t, sb)
	brokerEmpty(t, sc)
	if e := ha.Publish(event); e != nil {
		t.Fatal(e)
	}
	brokerReceive(t, sa, a)
	brokerEmpty(t, sb)
	brokerEmpty(t, sc)
	brokerEmpty(t, legacy)
	if e := hb.Publish(brokerStamped(t, other)); e != nil {
		t.Fatal(e)
	}
	brokerReceive(t, sb, other)
	if e := hc.Publish(event); e != nil {
		t.Fatal(e)
	}
	brokerReceive(t, sc, a)
	b.CloseRun("r")
	if b.SubscriberCount("r") != 0 {
		t.Fatal("legacy count")
	}
	if e := ha.Publish(event); e != nil {
		t.Fatal(e)
	}
	brokerReceive(t, sa, a)
}
func TestObservedBrokerCopiedLeaseAndReplacement(t *testing.T) {
	b := NewEventBroker()
	o := brokerOrigin(t, "t", "A")
	h1, h2 := brokerScope(t, b, o), brokerScope(t, b, o)
	old := brokerSub(t, h1)
	other := brokerSub(t, h2)
	copied := *h1
	if e := copied.Close(); e != nil {
		t.Fatal(e)
	}
	h1.Close()
	if e := h2.Publish(brokerStamped(t, o)); e != nil {
		t.Fatal("H2 retired by copied double close", e)
	}
	brokerReceive(t, old, o)
	brokerReceive(t, other, o)
	for _, h := range []*ObservedEventScope{h1, &copied} {
		if _, e := h.Subscribe(); !errors.Is(e, store.ErrRunDataCapability) {
			t.Fatal(e)
		}
		if e := h.Publish(brokerStamped(t, o)); !errors.Is(e, store.ErrRunDataCapability) {
			t.Fatal(e)
		}
	}
	h2.Close()
	next := brokerScope(t, b, o)
	fresh := brokerSub(t, next)
	old.Cancel()
	other.Cancel()
	copied.Close()
	h2.Close()
	if e := next.Publish(brokerStamped(t, o)); e != nil {
		t.Fatal(e)
	}
	brokerReceive(t, fresh, o)
}
func TestObservedBrokerValidationDropsAndAlert(t *testing.T) {
	b := NewEventBroker()
	o := brokerOrigin(t, "t", "A")
	h := brokerScope(t, b, o)
	event := brokerStamped(t, o)
	absent, e := store.CaptureRunObservation(store.RunDataHeader{RunID: "r", Format: 1}, "t", false)
	if e != nil {
		t.Fatal(e)
	}
	for _, original := range []store.RunObservation{{}, absent} {
		if _, e = b.AcquireObserved(original); !errors.Is(e, store.ErrRunDataCapability) {
			t.Fatal(e)
		}
	}
	var nilBroker *EventBroker
	if _, e = nilBroker.AcquireObserved(o); !errors.Is(e, store.ErrRunDataCapability) {
		t.Fatal(e)
	}
	for _, scope := range []*ObservedEventScope{nil, {}} {
		if _, e = scope.Subscribe(); !errors.Is(e, store.ErrRunDataCapability) {
			t.Fatal(e)
		}
		if e = scope.Publish(event); !errors.Is(e, store.ErrRunDataCapability) {
			t.Fatal(e)
		}
		if e = scope.Close(); e != nil {
			t.Fatal(e)
		}
	}
	// Refuse invalid input before subscriber count/drop decisions.
	if e = h.Publish(store.Event{RunID: "r"}); !errors.Is(e, store.ErrRunDataCapability) {
		t.Fatal(e)
	}
	wrong := event
	wrong.RunID = "wrong"
	if e = h.Publish(wrong); !errors.Is(e, store.ErrRunDataIdentityChanged) {
		t.Fatal(e)
	}
	if e = h.Publish(event); e != nil {
		t.Fatal(e)
	}
	sub := brokerSub(t, h)
	event.Type = store.EventAlert
	event.Seq = 0
	if e = h.Publish(event); e != nil {
		t.Fatal(e)
	}
	brokerReceive(t, sub, o)
	for i := 0; i < subscriberBufferSize+7; i++ {
		if e = h.Publish(event); e != nil {
			t.Fatal(e)
		}
	}
	if sub.Drops() != 7 {
		t.Fatal(sub.Drops())
	}
	if e = h.Publish(store.Event{RunID: "r"}); !errors.Is(e, store.ErrRunDataCapability) || sub.Drops() != 7 {
		t.Fatal(e, sub.Drops())
	}
	h.Close()
	count := 0
	for range sub.C {
		count++
	}
	if count != subscriberBufferSize {
		t.Fatal("buffer drained on close", count)
	}
}
func TestObservedBrokerRaceClosePublishCancel(t *testing.T) {
	for i := 0; i < 32; i++ {
		b := NewEventBroker()
		o := brokerOrigin(t, "t", "A")
		h := brokerScope(t, b, o)
		sub := brokerSub(t, h)
		event := brokerStamped(t, o)
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(3)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 512; j++ {
				if e := h.Publish(event); e != nil && !errors.Is(e, store.ErrRunDataCapability) {
					t.Error(e)
				}
			}
		}()
		go func() { defer wg.Done(); <-start; h.Close() }()
		go func() { defer wg.Done(); <-start; sub.Cancel() }()
		close(start)
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("close/publish/cancel failed to join")
		}
		for e := range sub.C {
			got, known := e.EventObservation()
			if !known || got != o {
				t.Fatal(got)
			}
		}
	}
}

func TestObservedBrokerUnknownCannotFallback(t *testing.T) {
	b := NewEventBroker()
	o := brokerOrigin(t, "t", "A")
	h := brokerScope(t, b, o)
	sub := brokerSub(t, h)
	if e := h.Publish(brokerStamped(t, o)); e != nil {
		t.Fatal(e)
	}
	brokerReceive(t, sub, o)
	if e := h.Publish(store.Event{RunID: "r", Type: store.EventAlert}); !errors.Is(e, store.ErrRunDataCapability) {
		t.Error("unknown fallback accepted", e)
	}
	brokerEmpty(t, sub)
	if e := h.Publish(brokerStamped(t, o)); e != nil {
		t.Fatal(e)
	}
	brokerReceive(t, sub, o)
}
func TestObservedBrokerEventValuesNotShared(t *testing.T) {
	o := brokerOrigin(t, "t", "A")
	h := brokerScope(t, NewEventBroker(), o)
	a, c := brokerSub(t, h), brokerSub(t, h)
	event := brokerStamped(t, o)
	event.NodeID = "original"
	event.Data = map[string]any{"borrowed": "read-only"}
	if e := h.Publish(event); e != nil {
		t.Fatal(e)
	}
	first, second := <-a.C, <-c.C
	if first == second {
		t.Fatal("event pointer shared")
	}
	first.NodeID = "changed"
	if second.NodeID != "original" {
		t.Fatal(second.NodeID)
	}
	// Referenced Data is deliberately not mutated: deep isolation is not promised.
	got, known := second.EventObservation()
	if !known || got != o {
		t.Fatal(got)
	}
}

func TestObservedBrokerKnownMismatchAndHistoricalFormats(t *testing.T) {
	b := NewEventBroker()
	a := brokerOrigin(t, "tenantA", "birthA")
	h := brokerScope(t, b, a)
	sub := brokerSub(t, h)
	wrongBirth := brokerOrigin(t, "tenantA", "birthB")
	wrongTenant := brokerOrigin(t, "tenantB", "birthA")
	for _, o := range []store.RunObservation{wrongBirth, wrongTenant} {
		if e := h.Publish(brokerStamped(t, o)); !errors.Is(e, store.ErrRunDataIdentityChanged) {
			t.Error("known mismatched origin accepted", e)
		}
		brokerEmpty(t, sub)
		if sub.Drops() != 0 {
			t.Fatal("invalid origin counted as slow delivery", sub.Drops())
		}
	}
	if e := h.Publish(brokerStamped(t, a)); e != nil {
		t.Fatal(e)
	}
	brokerReceive(t, sub, a)
	// This tests represented broker identity, not backend protocol admission.
	// Present historical records without a birth retain their documented limit.
	legacy := brokerOrigin(t, "tenantA", "")
	exact, e := store.CaptureRunObservation(store.RunDataHeader{RunID: "r", Format: 2}, "tenantA", true)
	if e != nil {
		t.Fatal(e)
	}
	historical := brokerScope(t, b, legacy)
	precise := brokerScope(t, b, exact)
	ls, es := brokerSub(t, historical), brokerSub(t, precise)
	if e = historical.Publish(brokerStamped(t, legacy)); e != nil {
		t.Fatal(e)
	}
	brokerReceive(t, ls, legacy)
	brokerEmpty(t, es)
	if e = precise.Publish(brokerStamped(t, exact)); e != nil {
		t.Fatal(e)
	}
	brokerReceive(t, es, exact)
	brokerEmpty(t, ls)
	if e = historical.Publish(brokerStamped(t, exact)); !errors.Is(e, store.ErrRunDataIdentityChanged) {
		t.Error("different format accepted", e)
	}
	brokerEmpty(t, ls)
	brokerEmpty(t, es)
	if ls.Drops() != 0 || es.Drops() != 0 {
		t.Fatal("mismatch changed drops", ls.Drops(), es.Drops())
	}
}

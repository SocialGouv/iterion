package runview

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// No entry state is seeded: this schedules an already legitimate subscription
// cancellation between client-map removal and its captured broker cancellation.
func TestBridgeReviewCancellationWindowWithBrokerOutsider(t *testing.T) {
	r, _ := bridgeRegistry(t)
	o := bridgeOrigin(t)
	s1, s2 := bridgeSub(), bridgeSub()
	var opened atomic.Int32
	r.open = func(context.Context) (runstream.Source, error) {
		s := s1
		if opened.Add(1) > 1 {
			s = s2
		}
		return &bridgeProbeSource{subscribe: func(context.Context) (runstream.EventSubscription, error) { return s, nil }}, nil
	}
	external, e := r.broker.AcquireObserved(o)
	if e != nil {
		t.Fatal(e)
	}
	h1, e := r.acquire(t.Context(), o)
	if e != nil {
		t.Fatal(e)
	}
	entered, release, closeDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var unlock sync.Once
	originalCancel := h1.client.sub.cancel
	h1.client.sub.cancel = func() { close(entered); <-release; originalCancel() }
	var h2 *observedBridgeLease
	defer func() {
		unlock.Do(func() { close(release) })
		select {
		case <-closeDone:
		case <-time.After(2 * time.Second):
			t.Error("old Close did not join")
		}
		if h2 != nil {
			h2.Close()
		}
		external.Close()
	}()
	go func() { h1.Close(); close(closeDone) }()
	bridgeWait(t, entered)
	// Owner Close is paused inside its legitimate subscription Cancel. EOF is
	// independent and asks the worker to complete while that cancellation waits.
	s1.eof()
	select {
	case <-h1.Done():
		// Vulnerable path: replacement may start before ALL H1 subscriptions close.
		h2, e = r.acquire(t.Context(), o)
		if e != nil {
			t.Fatal(e)
		}
		evt := bridgeEvent(t, o)
		evt.NodeID = "replacement"
		s2.events <- []*store.Event{evt}
		bridgeReceive(t, h2, o)
		select {
		case old, ok := <-h1.Events():
			if ok && old != nil && old.NodeID == "replacement" {
				t.Fatal("H2 reached old H1 client while its cancellation was pending")
			}
			if ok {
				t.Fatal("unexpected old event", old)
			}
		default:
			t.Fatal("H1 completed before its owned subscription closed")
		}
	case <-time.After(100 * time.Millisecond):
		// Fixed path: the retiring barrier waits for cancellation completion.
		if opened.Load() != 1 {
			t.Fatal("replacement opened before canceled subscription closed")
		}
		unlock.Do(func() { close(release) })
		bridgeWait(t, h1.Done())
		bridgeWait(t, closeDone)
		h2, e = r.acquire(t.Context(), o)
		if e != nil {
			t.Fatal(e)
		}
		s2.events <- []*store.Event{bridgeEvent(t, o)}
		bridgeReceive(t, h2, o)
		bridgeClosed(t, h1)
	}
}

package runview

import (
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestBroker_FanOutToTwoSubscribers(t *testing.T) {
	b := NewEventBroker()
	a := b.Subscribe("run-1")
	c := b.Subscribe("run-1")
	defer a.Cancel()
	defer c.Cancel()

	b.Publish(store.Event{Seq: 0, Type: store.EventRunStarted, RunID: "run-1"})
	b.Publish(store.Event{Seq: 1, Type: store.EventRunFinished, RunID: "run-1"})

	for i, sub := range []*EventSubscription{a, c} {
		for n := 0; n < 2; n++ {
			select {
			case e := <-sub.C:
				if e == nil {
					t.Fatalf("sub %d: nil event", i)
				}
				if e.Seq != int64(n) {
					t.Errorf("sub %d event %d Seq = %d, want %d", i, n, e.Seq, n)
				}
			case <-time.After(time.Second):
				t.Fatalf("sub %d timed out waiting for event %d", i, n)
			}
		}
	}
}

func TestBroker_RoutesByRunID(t *testing.T) {
	b := NewEventBroker()
	subA := b.Subscribe("run-A")
	subB := b.Subscribe("run-B")
	defer subA.Cancel()
	defer subB.Cancel()

	b.Publish(store.Event{Seq: 0, RunID: "run-A", Type: store.EventNodeStarted})
	b.Publish(store.Event{Seq: 0, RunID: "run-B", Type: store.EventNodeStarted})

	select {
	case e := <-subA.C:
		if e.RunID != "run-A" {
			t.Errorf("subA got run %q", e.RunID)
		}
	case <-time.After(time.Second):
		t.Fatalf("subA timed out")
	}
	select {
	case e := <-subB.C:
		if e.RunID != "run-B" {
			t.Errorf("subB got run %q", e.RunID)
		}
	case <-time.After(time.Second):
		t.Fatalf("subB timed out")
	}
}

func TestBroker_CancelStopsDelivery(t *testing.T) {
	b := NewEventBroker()
	sub := b.Subscribe("run-1")
	sub.Cancel()
	// Publish should not deadlock and should not deliver to a
	// cancelled subscriber.
	b.Publish(store.Event{RunID: "run-1"})
	if c := b.SubscriberCount("run-1"); c != 0 {
		t.Errorf("SubscriberCount = %d, want 0", c)
	}
	// Channel must be closed.
	select {
	case _, ok := <-sub.C:
		if ok {
			t.Errorf("channel delivered after Cancel")
		}
	case <-time.After(time.Second):
		t.Fatalf("channel not closed after Cancel")
	}
}

func TestBroker_CloseRunDrainsAllSubs(t *testing.T) {
	b := NewEventBroker()
	subs := []*EventSubscription{
		b.Subscribe("run-1"),
		b.Subscribe("run-1"),
		b.Subscribe("run-1"),
	}
	b.CloseRun("run-1")
	for i, sub := range subs {
		select {
		case _, ok := <-sub.C:
			if ok {
				t.Errorf("sub %d: channel delivered after CloseRun", i)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d: channel not closed", i)
		}
	}
	if c := b.SubscriberCount("run-1"); c != 0 {
		t.Errorf("SubscriberCount = %d, want 0", c)
	}
}

func TestBroker_LossyDropOnFullBuffer(t *testing.T) {
	b := NewEventBroker()
	sub := b.Subscribe("run-1")
	defer sub.Cancel()

	// Publish more than the buffer size without draining.
	const overflow = subscriberBufferSize * 2
	for i := 0; i < overflow; i++ {
		b.Publish(store.Event{Seq: int64(i), RunID: "run-1"})
	}
	if drops := sub.Drops(); drops == 0 {
		t.Errorf("Drops = 0, expected > 0 with %d publishes against buffer %d", overflow, subscriberBufferSize)
	}
}

func TestBroker_ConcurrentPublishSubscribe(t *testing.T) {
	// Smoke test for the read/write lock discipline: many concurrent
	// publishes interleaved with subscribe/cancel should never panic
	// or deadlock.
	b := NewEventBroker()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Publishers.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.Publish(store.Event{RunID: "run-1"})
				}
			}
		}()
	}
	// Subscribers churning.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					sub := b.Subscribe("run-1")
					sub.Cancel()
				}
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

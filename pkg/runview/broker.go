package runview

import (
	"sync"
	"sync/atomic"

	"github.com/SocialGouv/iterion/pkg/store"
)

// subscriberBufferSize is the per-subscriber channel buffer. A slow
// consumer that fills its buffer is dropped (lossy fan-out): the WS
// transport is best-effort and the client can recover by re-subscribing
// with from_seq, since events.jsonl on disk is the source of truth.
// Blocking the publisher is not an option — it would back up onto the
// runtime goroutine that just persisted the event.
const subscriberBufferSize = 256

// EventBroker is the in-process fan-out for persistent run events. The
// runtime publishes via runtime.WithEventObserver(broker.Publish); WS
// handlers subscribe via broker.Subscribe(runID).
//
// The broker does NOT read from disk. Callers that need historical
// events (catch-up replay) should LoadEvents(runID) first, then drain
// the live channel and dedup by seq — see Service.Subscribe for the
// canonical recipe.
type EventBroker struct {
	mu          sync.RWMutex
	subscribers map[string][]*eventSub // run_id → active subscribers
}

// NewEventBroker creates an empty broker.
func NewEventBroker() *EventBroker {
	return &EventBroker{subscribers: make(map[string][]*eventSub)}
}

// eventSub is one active subscription. The drops counter is incremented
// when the channel buffer is full and an event is discarded; clients
// can detect drops by seq gaps and recover via re-subscribe.
//
// drops is read concurrently via Drops() while Publish increments it
// under the broker's read lock — atomic to avoid the data race the
// race detector would otherwise flag.
type eventSub struct {
	ch     chan *store.Event
	closed bool
	drops  atomic.Int32
}

// EventSubscription is the public handle returned to callers. Receive
// from C, call Cancel to unsubscribe (idempotent).
type EventSubscription struct {
	C       <-chan *store.Event
	cancel  func()
	once    sync.Once
	dropsFn func() int
}

// Cancel unregisters the subscription and closes the channel.
// Idempotent — safe to call multiple times.
func (s *EventSubscription) Cancel() {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
}

// Drops reports the number of events that were dropped because this
// subscriber's buffer was full. The transport layer can surface this
// to the client so the UI knows to re-subscribe with from_seq.
func (s *EventSubscription) Drops() int {
	if s.dropsFn == nil {
		return 0
	}
	return s.dropsFn()
}

// Subscribe registers a new subscriber for runID and returns its
// handle. The subscription only delivers events received AFTER this
// call returns; for catch-up + live replay use Service.Subscribe.
func (b *EventBroker) Subscribe(runID string) *EventSubscription {
	sub := &eventSub{ch: make(chan *store.Event, subscriberBufferSize)}

	b.mu.Lock()
	b.subscribers[runID] = append(b.subscribers[runID], sub)
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		list := b.subscribers[runID]
		for i, s := range list {
			if s == sub {
				b.subscribers[runID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(b.subscribers[runID]) == 0 {
			delete(b.subscribers, runID)
		}
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
	return &EventSubscription{
		C:       sub.ch,
		cancel:  cancel,
		dropsFn: func() int { return int(sub.drops.Load()) },
	}
}

// Publish fans out an event to every active subscriber for evt.RunID.
// Safe for concurrent use. This is the function passed to
// runtime.WithEventObserver. Slow subscribers are dropped lossily —
// the publisher never blocks (see subscriberBufferSize for the
// rationale).
//
// The read lock is held through the entire fan-out, including the
// non-blocking channel sends. This serialises against Cancel and
// CloseRun (which take the write lock to close subscriber channels),
// preventing the "send on closed channel" race that would otherwise
// occur if Cancel ran between Publish's slice copy and its send.
// Because every send is wrapped in a `select { default }`, the read
// lock is held only briefly even under buffer pressure.
//
// Publish takes a value to match the runtime.WithEventObserver
// signature; we copy the pointer-shaped fields on the way out so
// downstream consumers receive a stable snapshot of the event even if
// the runtime mutated its local copy after emit.
func (b *EventBroker) Publish(evt store.Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	subs := b.subscribers[evt.RunID]
	if len(subs) == 0 {
		return
	}

	// Heap-allocate one Event per publish so each subscriber can
	// hold its own pointer without aliasing. The cost is negligible
	// vs the JSON marshalling that already happened upstream.
	out := evt
	for _, sub := range subs {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- &out:
		default:
			sub.drops.Add(1)
		}
	}
}

// CloseRun terminates every subscription for runID. Used at run
// completion to signal that no further events will arrive on this
// stream. After CloseRun the broker forgets about the run; new
// Subscribe calls would receive a fresh empty subscription that never
// fires (callers should use cold reads for terminated runs).
func (b *EventBroker) CloseRun(runID string) {
	b.mu.Lock()
	subs := b.subscribers[runID]
	delete(b.subscribers, runID)
	b.mu.Unlock()

	for _, sub := range subs {
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
}

// SubscriberCount returns the live subscriber count for runID. Useful
// for tests and metrics.
func (b *EventBroker) SubscriberCount(runID string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers[runID])
}

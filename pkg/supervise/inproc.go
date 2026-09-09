package supervise

import (
	"context"
	"fmt"
	"sync"

	"github.com/SocialGouv/iterion/pkg/store"
)

// EventHub is an in-process Observer fed directly by the runtime
// engine's event stream (runtime.WithEventObserver) — for CLI
// `iterion run` supervision, where there is no runview broker. The
// engine calls Publish for every event; each subscribing Coordinator
// gets its own fan-out channel via ObserveRun.
type EventHub struct {
	mu   sync.Mutex
	subs map[chan *store.Event]struct{}
}

// NewEventHub creates an empty hub.
func NewEventHub() *EventHub {
	return &EventHub{subs: make(map[chan *store.Event]struct{})}
}

// Publish fans an event out to every subscriber, non-blocking (a slow
// supervisor drops events rather than stalling the engine goroutine).
// Matches the runtime.WithEventObserver signature.
func (h *EventHub) Publish(evt store.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		e := evt
		select {
		case ch <- &e:
		default:
		}
	}
}

// ObserveRun implements the Observer seam — registers a new fan-out
// channel. runID is ignored (a CLI run has a single run on the hub).
func (h *EventHub) ObserveRun(_ context.Context, _ string) (<-chan *store.Event, func(), error) {
	ch := make(chan *store.Event, subscriberBufferSize)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			close(ch)
			h.mu.Unlock()
		})
	}
	return ch, release, nil
}

// StoreInjector is an Injector that appends node-scoped steering
// messages straight onto a RunStore — used for in-process supervision
// (CLI `iterion run`) where sharing the engine's store handle keeps the
// inbox doorbell in lockstep without a runview.Service.
type StoreInjector struct {
	Store   store.RunStore
	Publish func(store.Event) // optional broker fan-out; nil persists only
}

// WatcherProgressStore exposes the same durable run store to the coordinator
// for its optional anti-loop cursor. It is intentionally a capability method;
// callers using another Injector remain source-compatible and simply run with
// the in-memory cooldown.
func (i *StoreInjector) WatcherProgressStore() store.RunStore {
	if i == nil {
		return nil
	}
	return i.Store
}

// Inject implements the Injector seam: append a queued message tagged
// with nodeID (so the engine's drain delivers it only while that node is
// active), and emit user_message_queued so the run console reflects it.
// A terminal run is refused — runview.Service.QueueMessage has the same
// guard, and without it an eval finishing after the run's end would park
// a stale steering message that the NEXT resume/redelivery drains into a
// fresh pass.
func (i *StoreInjector) Inject(ctx context.Context, runID, nodeID, text string) error {
	return i.inject(ctx, runID, nodeID, text, newInboxMessageID(), false)
}

// InjectOnce writes a supervisor delivery under a stable ID. Replaying the
// same trigger is a no-op even when the earlier message was already consumed.
func (i *StoreInjector) InjectOnce(ctx context.Context, runID, nodeID, text, deliveryID string) error {
	return i.inject(ctx, runID, nodeID, text, deliveryID, true)
}

func (i *StoreInjector) inject(ctx context.Context, runID, nodeID, text, messageID string, once bool) error {
	if r, err := i.Store.LoadRun(ctx, runID); err == nil && r != nil {
		switch r.Status {
		case store.RunStatusFinished, store.RunStatusFailed, store.RunStatusCancelled:
			return fmt.Errorf("supervise: run %s is %s — steering message refused", runID, r.Status)
		}
	}
	msg := store.QueuedUserMessage{ID: messageID, Text: text, NodeID: nodeID}
	inserted := true
	var err error
	if once {
		onceStore := store.AsQueuedMessageInsertOnceStore(i.Store)
		if onceStore == nil {
			// A RunStore decorator can hide the optional atomic capability. The
			// durable watcher cursor still suppresses ordinary replays, so degrade
			// to at-least-once delivery instead of dropping the intervention.
			err = i.Store.AppendQueuedMessage(ctx, runID, msg)
		} else {
			inserted, err = onceStore.AppendQueuedMessageOnce(ctx, runID, msg)
		}
	} else {
		err = i.Store.AppendQueuedMessage(ctx, runID, msg)
	}
	if err != nil {
		return err
	}
	if inserted {
		_ = store.NormalizeQueuedForAppend(&msg, runID)
		store.PublishInboxEvent(ctx, i.Store, i.Publish, store.EventUserMessageQueued, runID, msg)
	}
	return nil
}

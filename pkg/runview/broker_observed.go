package runview

import (
	"github.com/SocialGouv/iterion/pkg/store"
	"slices"
)

// ObservedEventScope is an owner lease in one broker instance. Copies share
// the private lease state; separate acquisitions have separate owner leases.
// Origin is routing provenance, not authorization or an execution epoch.
type ObservedEventScope struct{ lease *observedEventLease }
type observedEventLease struct {
	broker *EventBroker
	topic  *observedEventTopic
	closed bool // all mutable state below is protected by broker.mu
}
type observedEventTopic struct {
	original    store.RunObservation
	owners      int
	closed      bool
	subscribers []*eventSub
}

// AcquireObserved admits a present original only. Legacy orphan subscriptions
// remain available through Subscribe; an absent request partition is not stored
// tenant isolation. Equal observations share a live topic in this broker only.
func (b *EventBroker) AcquireObserved(original store.RunObservation) (*ObservedEventScope, error) {
	if b == nil || !original.Observed() || !original.ParentPresent() {
		return nil, store.ErrRunDataCapability
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.observed == nil {
		b.observed = make(map[store.RunObservation]*observedEventTopic)
	}
	topic := b.observed[original]
	if topic == nil {
		topic = &observedEventTopic{original: original}
		b.observed[original] = topic
	}
	topic.owners++
	return &ObservedEventScope{lease: &observedEventLease{broker: b, topic: topic}}, nil
}

// Subscribe adds no owner reference. It lives until cancellation or the final
// topic owner releases. Nil/zero/closed scopes refuse without side effects.
func (h *ObservedEventScope) Subscribe() (*EventSubscription, error) {
	if h == nil || h.lease == nil {
		return nil, store.ErrRunDataCapability
	}
	lease := h.lease
	b := lease.broker
	b.mu.Lock()
	defer b.mu.Unlock()
	if lease.closed || lease.topic.closed {
		return nil, store.ErrRunDataCapability
	}
	topic := lease.topic
	sub := &eventSub{ch: make(chan *store.Event, subscriberBufferSize)}
	topic.subscribers = append(topic.subscribers, sub)
	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if i := slices.Index(topic.subscribers, sub); i >= 0 {
			topic.subscribers = slices.Delete(topic.subscribers, i, i+1)
		}
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
	return &EventSubscription{C: sub.ch, cancel: cancel, dropsFn: func() int { return int(sub.drops.Load()) }}, nil
}

// Publish validates identity even with no subscribers. It never publishes to
// legacy subscribers. Event values are copied; referenced payloads are borrowed
// and read-only, not deeply cloned. Sends remain nonblocking and lossy.
func (h *ObservedEventScope) Publish(evt store.Event) error {
	if h == nil || h.lease == nil {
		return store.ErrRunDataCapability
	}
	lease := h.lease
	b := lease.broker
	b.mu.RLock()
	defer b.mu.RUnlock()
	if lease.closed || lease.topic.closed {
		return store.ErrRunDataCapability
	}
	original, known := evt.EventObservation()
	if !known {
		return store.ErrRunDataCapability
	}
	if original != lease.topic.original || evt.RunID != lease.topic.original.Header().RunID {
		return store.ErrRunDataIdentityChanged
	}
	for _, sub := range lease.topic.subscribers {
		if sub.closed {
			continue
		}
		out := evt
		select {
		case sub.ch <- &out:
		default:
			sub.drops.Add(1)
		}
	}
	return nil
}

// Close releases this owner exactly once, including through copied handles.
// Nil/zero/already-closed handles are harmless. The final owner closes the
// topic's channels, but already buffered events remain readable. A Publish
// which wins the mutex first may enqueue before Close linearizes.
func (h *ObservedEventScope) Close() error {
	if h == nil || h.lease == nil {
		return nil
	}
	lease := h.lease
	b := lease.broker
	b.mu.Lock()
	defer b.mu.Unlock()
	if lease.closed {
		return nil
	}
	lease.closed = true
	topic := lease.topic
	topic.owners--
	if topic.owners != 0 {
		return nil
	}
	topic.closed = true
	if b.observed[topic.original] == topic {
		delete(b.observed, topic.original)
	}
	for _, sub := range topic.subscribers {
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
	topic.subscribers = nil
	return nil
}

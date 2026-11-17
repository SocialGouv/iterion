package runview

import (
	"fmt"
	"slices"
	"sync"

	"github.com/SocialGouv/iterion/pkg/alert"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The alert registry: alert-only topics keyed by registration, one epoch
// per adoption. See the type comments below.

type alertRegistry struct {
	mu      sync.Mutex
	topics  map[store.RunObservation]*alertTopic
	waiters map[string]map[*alertWaiter]struct{}
	next    uint64
}

// alertWaiter is one fetch's slot in a run ID's broadcast set (F4/R4): the
// launch publishes Active before its registration, so a setup-time fetch
// that misses waits on this channel until acquire or release broadcasts.
type alertWaiter struct {
	ch chan struct{}
}

// alertTopic is one registration's epoch: the subscription set of its lane
// and the holder count of its bails. It closes when the LAST bail closes —
// a holder's release never ends a lane another holder still lives on (H5) —
// regardless of any subscriber refcount (F6): subscriptions are non-owning.
type alertTopic struct {
	obs    store.RunObservation
	epoch  uint64
	subs   []*eventSub
	bails  int
	closed bool
}

func newAlertRegistry() *alertRegistry {
	return &alertRegistry{
		topics:  make(map[store.RunObservation]*alertTopic),
		waiters: make(map[string]map[*alertWaiter]struct{}),
	}
}

// acquire hands out one bail on obs's CURRENT epoch: a live topic gains a
// bail (another holder joins — H5), an absent or closed one gains a fresh
// epoch, broadcast so waiting fetches re-arm. Any previous topic for the
// same observation — a re-adoption after release — is replaced as the
// CURRENT one; the old epoch was closed at its last bail and its merge
// subscription drains through the retired queue (F14).
func (r *alertRegistry) acquire(runID string, obs store.RunObservation) *alertLane {
	r.mu.Lock()
	tp := r.topics[obs]
	if tp == nil || tp.closed {
		r.next++
		tp = &alertTopic{obs: obs, epoch: r.next}
		r.topics[obs] = tp
		r.broadcastLocked(runID)
	}
	tp.bails++
	r.mu.Unlock()
	return &alertLane{reg: r, runID: runID, topic: tp}
}

// publish validates identity and class, then fans out to the epoch's
// subscribers. The registry is the manager's lane target only: it refuses
// a non-alert frame and a replayed persisted row (TR3/M9a/M9b), and a
// stamp that is not the lane's own registration (identity+epoch check).
func (r *alertRegistry) publish(lane *alertLane, evt store.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	tp := lane.topic
	if tp == nil || tp.closed {
		return alert.ErrNotRegistered
	}
	if evt.Type != store.EventAlert || evt.Seq != 0 {
		return fmt.Errorf("registry publish run=%s type=%s seq=%d: %w", evt.RunID, evt.Type, evt.Seq, alert.ErrNotRegistered)
	}
	stamp, stamped := evt.EventObservation()
	if !stamped || stamp != tp.obs {
		return fmt.Errorf("registry publish run=%s: stamp %v does not match the lane's registration: %w", evt.RunID, stamp, alert.ErrNotRegistered)
	}
	// Same lossy fan-out as the broker: a slow merge is dropped (buffered
	// frames on all lanes stay bounded), the publisher never blocks.
	for _, sub := range tp.subs {
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

// broadcastLocked wakes every fetch waiting on runID's registration state.
func (r *alertRegistry) broadcastLocked(runID string) {
	for w := range r.waiters[runID] {
		select {
		case w.ch <- struct{}{}:
		default:
		}
	}
}

// waitSlot registers a waiter for runID and returns its signal channel.
func (r *alertRegistry) waitSlot(runID string) *alertWaiter {
	w := &alertWaiter{ch: make(chan struct{}, 1)}
	r.mu.Lock()
	if r.waiters[runID] == nil {
		r.waiters[runID] = make(map[*alertWaiter]struct{})
	}
	r.waiters[runID][w] = struct{}{}
	r.mu.Unlock()
	return w
}

func (r *alertRegistry) dropSlot(runID string, w *alertWaiter) {
	r.mu.Lock()
	if set := r.waiters[runID]; set != nil {
		delete(set, w)
		if len(set) == 0 {
			delete(r.waiters, runID)
		}
	}
	r.mu.Unlock()
}

// subscribe returns a non-owning subscription to obs's CURRENT epoch, or
// nil when it has none. The F4 wait lives in the service's fetch, where the
// manager's registration state is reachable.
func (r *alertRegistry) subscribe(obs store.RunObservation) (*EventSubscription, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tp := r.topics[obs]
	if tp == nil || tp.closed {
		return nil, false
	}
	sub := &eventSub{ch: make(chan *store.Event, subscriberBufferSize)}
	tp.subs = append(tp.subs, sub)
	cancel := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if i := slices.Index(tp.subs, sub); i >= 0 {
			tp.subs = slices.Delete(tp.subs, i, i+1)
		}
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
	return &EventSubscription{C: sub.ch, cancel: cancel, dropsFn: func() int { return int(sub.drops.Load()) }}, true
}

// alertLane is the ObservedLane handle the registration hands the alert
// manager: publishes validate the epoch, Close ends it (the registration's
// release) and broadcasts so waiting fetches re-arm (F4).
type alertLane struct {
	reg   *alertRegistry
	runID string
	topic *alertTopic
}

func (l *alertLane) Publish(evt store.Event) error {
	if l == nil || l.reg == nil || l.topic == nil {
		return alert.ErrNotRegistered
	}
	return l.reg.publish(l, evt)
}

func (l *alertLane) Close() error {
	if l == nil || l.reg == nil || l.topic == nil {
		return nil
	}
	r := l.reg
	r.mu.Lock()
	defer r.mu.Unlock()
	tp := l.topic
	if tp.closed {
		return nil
	}
	// H5: the lane is one bail. The epoch ends with the LAST bail — a
	// holder's release never takes the delivery lane down for the holders
	// still on it.
	if tp.bails > 0 {
		tp.bails--
	}
	if tp.bails > 0 {
		return nil
	}
	tp.closed = true
	// The merge-side subscriptions are non-owning: closing their channels
	// (not cancelling) preserves the buffered frames for the retired-queue
	// drain (F14).
	for _, sub := range tp.subs {
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
	// Remove the epoch only while it is still the map's head: a newer
	// epoch of the same observation (a fast re-adoption) owns the slot.
	if cur := r.topics[tp.obs]; cur == tp {
		delete(r.topics, tp.obs)
	}
	r.broadcastLocked(l.runID)
	return nil
}

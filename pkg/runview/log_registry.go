package runview

import (
	"slices"
	"sync"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The log lane registry: one topic per producer registration, retained past
// its last bail until a subscriber observes its end or a newer epoch replaces
// it (R54 slice 4). A lane carries NO bytes — only wakes and the epoch's one
// end value — so a slow or absent subscriber never loses a byte to a full
// channel: the bytes stay in the store and a wake merely says "read again".

type logEnd uint8

const (
	// logEndNone is a wake, not a fin: the store may hold new bytes.
	logEndNone logEnd = iota
	// logEndTerminal is a producer that finished (and wrote its finalize).
	logEndTerminal
	// logEndPaused is a producer that parked on a human wait: the run is
	// dormant, its lane will be replaced by the resume's epoch.
	logEndPaused
	// logEndLost is a producer that vanished without a terminal status, or a
	// local capture failure: a typed error at the subscriber, never a settle.
	logEndLost
)

// logDurability says whether the epoch's bytes reached a run.log the store
// can read. A memory-only producer is refused at adoption.
type logDurability uint8

const (
	logMemoryOnly logDurability = iota
	logDurable
)

// LogWake is what a lane publishes: a payload-free wake (End == logEndNone)
// or the epoch's one end value. The channel closes right after the end.
type LogWake struct{ End logEnd }

type logRegistry struct {
	mu      sync.Mutex
	topics  map[store.RunObservation]*logTopic
	current map[string]*logTopic
	// waiters carry the bounded registration wait of observedLogLane.
	waiters map[string]map[*logWaiter]struct{}
	// watches carry the merge's immediate notification: an acquire, a close
	// and a recorded producer failure all signal them. Never replayed — a
	// subscriber installs its watch BEFORE its initial observation.
	watches map[string]map[chan struct{}]struct{}
	// failures retain a local producer's capture failure recorded when no
	// topic exists yet (the capture precedes the acquire). A failure recorded
	// against a live topic lives on the topic instead, so it survives the
	// bail like any closed epoch.
	failures map[string]*logFailure
	next     uint64
}

// logTopic is one registration's epoch: the wake subscribers of its lane, the
// holder count of its bails and the one end it will ever deliver. It stays in
// the registry after its last bail: a subscriber that parks across the close
// still reads the retained end (E2).
type logTopic struct {
	runID   string
	obs     store.RunObservation
	epoch   uint64
	subs    []*logWakeSub
	bails   int
	closed  bool
	end     logEnd
	durable bool
	// fail is the typed cause of a logEndLost, when one was recorded.
	fail error
}

// logLane is one holder's bail on an epoch. Wake publishes bytes are visible;
// Fail and Close end the epoch (first end wins).
type logLane struct {
	reg      *logRegistry
	runID    string
	topic    *logTopic
	released bool
}

type logWakeSub struct {
	ch     chan LogWake
	closed bool
}

// LogWakeSubscription is one subscriber's view of an epoch. Closed is true
// when the subscription was created on a retained, already-closed epoch: the
// retained end is already buffered on C and the channel is closed.
type LogWakeSubscription struct {
	C       <-chan LogWake
	Durable bool
	Closed  bool
	obs     store.RunObservation
	cancel  func()
}

// Cancel unregisters the subscription. Idempotent.
func (s *LogWakeSubscription) Cancel() {
	if s == nil || s.cancel == nil {
		return
	}
	s.cancel()
}

// logWaiter is one fetch's slot in a run ID's registration broadcast.
type logWaiter struct {
	ch chan struct{}
}

// logFailure is a producer's capture failure and the generation that recorded
// it: a late failure never replaces an end already settled on that generation.
type logFailure struct {
	err   error
	epoch uint64
}

func newLogRegistry() *logRegistry {
	return &logRegistry{
		topics:   make(map[store.RunObservation]*logTopic),
		current:  make(map[string]*logTopic),
		waiters:  make(map[string]map[*logWaiter]struct{}),
		watches:  make(map[string]map[chan struct{}]struct{}),
		failures: make(map[string]*logFailure),
	}
}

// acquire hands out one bail on obs's current epoch: a live topic gains a
// bail (another holder joins), an absent or closed one gains a fresh epoch,
// broadcast so parked subscribers re-arm. A closed epoch is never rejoined:
// its end was delivered and only a newer registration may replace it.
func (r *logRegistry) acquire(runID string, obs store.RunObservation, dur logDurability) *logLane {
	r.mu.Lock()
	tp := r.topics[obs]
	if tp == nil || tp.closed {
		r.next++
		tp = &logTopic{runID: runID, obs: obs, epoch: r.next, durable: dur == logDurable}
		r.topics[obs] = tp
		r.current[runID] = tp
		// The new generation supersedes a capture failure recorded before any
		// epoch existed: a fresh producer is a fresh start, not a doomed one.
		delete(r.failures, runID)
		r.notifyLocked(runID)
	} else if dur == logDurable {
		// One producer writes the canonical run.log; a durable holder settles
		// the epoch's durability for every subscriber.
		tp.durable = true
	}
	tp.bails++
	r.mu.Unlock()
	return &logLane{reg: r, runID: runID, topic: tp}
}

// subscribe returns a non-owning subscription to obs's current epoch — open
// or retained-closed. A closed epoch delivers its retained end immediately
// and closes the channel (Closed is true).
func (r *logRegistry) subscribe(obs store.RunObservation) (*LogWakeSubscription, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tp := r.topics[obs]
	if tp == nil {
		return nil, false
	}
	if tp.closed || tp.end != logEndNone {
		end := tp.end
		if end == logEndNone {
			// A closed epoch always carries an end; a stream nobody can vouch
			// for is lost, never implicitly complete.
			end = logEndLost
		}
		ch := make(chan LogWake, 1)
		ch <- LogWake{End: end}
		close(ch)
		return &LogWakeSubscription{C: ch, Durable: tp.durable, Closed: true, obs: obs, cancel: func() {}}, true
	}
	sub := &logWakeSub{ch: make(chan LogWake, 1)}
	tp.subs = append(tp.subs, sub)
	cancel := func() { r.cancelSub(tp, sub) }
	return &LogWakeSubscription{C: sub.ch, Durable: tp.durable, obs: obs, cancel: cancel}, true
}

// cancelSub unregisters one wake subscriber. Subscribers already closed by an
// end need no cancellation; their channel is gone.
func (r *logRegistry) cancelSub(tp *logTopic, sub *logWakeSub) {
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

// currentFor returns the CURRENT epoch of runID — open or retained-closed.
func (r *logRegistry) currentFor(runID string) (store.RunObservation, *logTopic, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tp := r.current[runID]
	if tp == nil {
		return store.RunObservation{}, nil, false
	}
	return tp.obs, tp, true
}

// watch returns a cap-1 channel signalled on every registration change of
// runID (acquire, close, recorded failure). The watch never replays the
// current state: install it before the initial observation.
func (r *logRegistry) watch(runID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	r.mu.Lock()
	if r.watches[runID] == nil {
		r.watches[runID] = make(map[chan struct{}]struct{})
	}
	r.watches[runID][ch] = struct{}{}
	r.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			r.mu.Lock()
			if set := r.watches[runID]; set != nil {
				delete(set, ch)
				if len(set) == 0 {
					delete(r.watches, runID)
				}
			}
			r.mu.Unlock()
		})
	}
	return ch, cancel
}

// waitSlot registers a waiter for runID's registration state and returns its
// signal channel.
func (r *logRegistry) waitSlot(runID string) *logWaiter {
	w := &logWaiter{ch: make(chan struct{}, 1)}
	r.mu.Lock()
	if r.waiters[runID] == nil {
		r.waiters[runID] = make(map[*logWaiter]struct{})
	}
	r.waiters[runID][w] = struct{}{}
	r.mu.Unlock()
	return w
}

func (r *logRegistry) dropSlot(runID string, w *logWaiter) {
	r.mu.Lock()
	if set := r.waiters[runID]; set != nil {
		delete(set, w)
		if len(set) == 0 {
			delete(r.waiters, runID)
		}
	}
	r.mu.Unlock()
}

// notifyLocked wakes every registration waiter and watch of runID.
func (r *logRegistry) notifyLocked(runID string) {
	for w := range r.waiters[runID] {
		select {
		case w.ch <- struct{}{}:
		default:
		}
	}
	for ch := range r.watches[runID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// recordFailure retains a local producer's capture failure. Against a live
// topic it is the epoch's end (logEndLost with its cause), published at once
// and kept by the topic after its bail; with no topic yet it is held by the
// registry until a topic carries it or the observer reads it. A failure
// recorded after the epoch's end was settled is ignored: the first end of a
// generation wins.
func (r *logRegistry) recordFailure(runID string, err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if tp := r.current[runID]; tp != nil {
		if tp.end != logEndNone {
			return
		}
		r.publishEndLocked(tp, logEndLost, err)
		delete(r.failures, runID)
	} else {
		r.next++
		r.failures[runID] = &logFailure{err: err, epoch: r.next}
	}
	r.notifyLocked(runID)
}

// failure returns the retained producer failure of runID, preferring the
// current topic's: a topic whose epoch is settled carries its own cause.
func (r *logRegistry) failure(runID string) (error, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tp := r.current[runID]; tp != nil {
		// The current epoch owns the generation: its own cause, or none. A
		// failure recorded before any epoch existed is superseded by it.
		if tp.fail != nil {
			return tp.fail, true
		}
		return nil, false
	}
	if f := r.failures[runID]; f != nil {
		return f.err, true
	}
	return nil, false
}

// failureEpoch returns the generation a retained failure is attached to: the
// epoch of the topic that recorded it, or a fresh generation when it was
// recorded before any epoch existed. A merge arbitrates a late failure
// against the epoch it already ended with this value.
func (r *logRegistry) failureEpoch(runID string) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tp := r.current[runID]; tp != nil {
		if tp.fail == nil {
			return 0, false
		}
		return tp.epoch, true
	}
	if f := r.failures[runID]; f != nil {
		return f.epoch, true
	}
	return 0, false
}

// clearFailure drops the registry-level copy of a failure once a retained
// topic carries it. A failure with no topic is retained: the producer's bail
// may end long before the failure is observed.
func (r *logRegistry) clearFailure(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tp := r.current[runID]; tp != nil && tp.fail != nil {
		delete(r.failures, runID)
	}
}

// wake publishes a payload-free wake to the epoch's open subscribers. The
// channel is cap-1, so a wake that arrives while one is pending is coalesced:
// a lost wake carries no truth.
func (r *logRegistry) wake(tp *logTopic) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tp.closed || tp.end != logEndNone {
		return
	}
	for _, sub := range tp.subs {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- LogWake{}:
		default:
		}
	}
}

// release ends one bail. The last bail publishes the epoch's end — the end is
// the producer's value, so a caller that passes logEndNone never silently
// completes the stream: it is lost. The topic is KEPT after the close.
func (r *logRegistry) release(lane *logLane, end logEnd) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lane.released {
		return
	}
	lane.released = true
	tp := lane.topic
	if tp.bails > 0 {
		tp.bails--
	}
	if tp.end != logEndNone {
		// The epoch's end was already delivered (a lane's Fail, a captured
		// failure): the first end of a generation wins.
		return
	}
	if tp.bails > 0 {
		// Another holder still lives on the epoch: it publishes the end.
		return
	}
	if end == logEndNone {
		end = logEndLost
	}
	r.publishEndLocked(tp, end, nil)
}

// publishEndLocked delivers the epoch's one end to every open subscriber and
// closes them. The topic stays registered: E2 retention.
func (r *logRegistry) publishEndLocked(tp *logTopic, end logEnd, cause error) {
	if tp.end != logEndNone {
		return
	}
	tp.end = end
	if cause != nil {
		tp.fail = cause
	}
	tp.closed = true
	for _, sub := range tp.subs {
		if sub.closed {
			continue
		}
		// A pending wake carries no truth: drop it so the end has room.
		select {
		case <-sub.ch:
		default:
		}
		sub.ch <- LogWake{End: end}
		sub.closed = true
		close(sub.ch)
	}
	tp.subs = nil
	r.notifyLocked(tp.runID)
}

// Wake reports that the producer's bytes are visible to the store.
func (l *logLane) Wake() {
	if l == nil || l.reg == nil || l.topic == nil {
		return
	}
	l.reg.wake(l.topic)
}

// Fail ends the epoch as logEndLost with its cause. Sticky: an end already
// delivered is not replaced.
func (l *logLane) Fail(err error) {
	if l == nil || l.reg == nil || l.topic == nil {
		return
	}
	l.reg.mu.Lock()
	defer l.reg.mu.Unlock()
	l.reg.publishEndLocked(l.topic, logEndLost, err)
}

// Close releases this holder's bail. The LAST bail publishes end (the topic
// is retained, never removed).
func (l *logLane) Close(end logEnd) {
	if l == nil || l.reg == nil || l.topic == nil {
		return
	}
	l.reg.release(l, end)
}

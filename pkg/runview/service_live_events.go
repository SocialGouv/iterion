package runview

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// liveEventStreams owns the event subscriptions StreamSource serves on the
// native filesystem store: each one is a retained FileSource subscription of
// its own — replay, cursor, validation against the run it was opened on, and
// the terminal final drain — merged with the run ID's legacy broker
// subscription, which contributes out-of-band alerts, wake hints and the
// producer's end-of-events barrier, and — for a bound subscription of a
// registered run — the registration's own alert registry lane, whose frames
// ship as one event class and dedup the legacy carrier. A broker payload
// other than an alert is never delivered.
//
// The Service owns the FileSource and every merge; the store is borrowed.
// stop cancels and joins them with ErrSourceClosed, then releases every
// alert registration the service still holds.
type liveEventStreams struct {
	svc   *Service
	files *runstream.FileSource

	mu     sync.Mutex
	closed bool
	next   uint64
	active map[uint64]*liveEventEntry
	// logActive holds the bound log merges: same registration discipline as
	// the event entries, joined by stop before the FileSource closes.
	logActive map[uint64]*liveLogEntry
	// logPending holds the bound log subscriptions still retrying their
	// admission: stop cancels and joins them like the merges.
	logPending map[uint64]*liveLogEntry
	// logEnds is the instrumented assertion of §3.1: the end source each log
	// merge applied and how many times it applied it, keyed by run ID.
	logEnds map[string]logEndRecord

	// beforeFinish, when set (tests), runs on the merge goroutine once the
	// retained subscription has ended, before the merge decides how its own
	// stream ends.
	beforeFinish func(ctx context.Context)
	// laneFirst, when set (tests), is asked at the top of every merge
	// iteration while the merge holds a registry lane: true leaves the broker
	// lane out of that iteration's select, so a closed registry lane is served
	// before the broker frames queued behind it. nil in production.
	laneFirst func() bool

	// Test seams of the bound log merge. They run on the merge (or subscribe)
	// goroutine and are nil in production.
	//
	// beforeLogStatusPoll runs before each terminal status read (E3).
	beforeLogStatusPoll func()
	// beforeLogSettleEnd runs after the E3 window and before the final status
	// and registry checks. A test can register a producer there while its
	// watch token is still pending.
	beforeLogSettleEnd func()
	// beforeLogObserved runs after the watch is installed and before the
	// initial observation: a test lands a registration and its close in that
	// window (the observation, or the pending watch token, must still see it).
	beforeLogObserved func()
	// afterLogObserved runs once the initial observation returned and before
	// the merge starts: a test parks there to register and close a short epoch
	// before the merge ever looks again.
	afterLogObserved func()
	// afterLogAdmission runs on a retried admission's goroutine once it was
	// admitted, before its subscription is served: a test lands a stop there.
	afterLogAdmission func(ctx context.Context)
}

// liveEventEntry is one registered merge: the run it streams, its
// cancellation, the Terminal hint of its retained subscription, and the
// channel closed once its cleanup (inner join, broker cancel, tailer release,
// unregistration) has run.
type liveEventEntry struct {
	runID    string
	cancel   context.CancelCauseFunc
	termHint chan struct{}
	done     chan struct{}
}

// initLiveEventStreams wires retained per-client event replay for the native
// filesystem store only: an injected StreamSource (cloud) or any other store,
// a wrapper of the native one included, keeps the legacy path.
func (s *Service) initLiveEventStreams() {
	if s.streamSrc != nil || s.broker == nil {
		return
	}
	native, ok := s.store.(*store.FilesystemRunStore)
	if !ok || native == nil {
		return
	}
	s.liveEvents = &liveEventStreams{
		svc:        s,
		files:      runstream.NewFileSource(native, "", s.logger),
		active:     make(map[uint64]*liveEventEntry),
		logActive:  make(map[uint64]*liveLogEntry),
		logPending: make(map[uint64]*liveLogEntry),
		logEnds:    make(map[string]logEndRecord),
	}
	if s.alertRegistry == nil {
		s.alertRegistry = newAlertRegistry()
	}
}

// subscribe opens one client's event stream of runID from position. ctx keeps
// the caller's binding: a bound context reads only the run it binds.
func (l *liveEventStreams) subscribe(ctx context.Context, runID string, position runstream.EventCursor) (runstream.EventSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := l.svc
	// An unbound subscription would wait for a missing run to appear: the
	// run is loaded first, so a missing one is refused as it always was.
	if _, bound := store.RunDataBindingFrom(ctx); !bound {
		if _, err := s.store.LoadRun(ctx, runID); err != nil {
			return nil, fmt.Errorf("runview: event subscription of %s: %w", runID, err)
		}
	}
	// Read once, by the subscriber: the producer never reads the package
	// variable a test may tighten after this subscription began.
	terminalPoll := svcEventTerminalPollInterval

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, runstream.ErrSourceClosed
	}
	c, cancel := context.WithCancelCause(ctx)
	l.next++
	id := l.next
	entry := &liveEventEntry{runID: runID, cancel: cancel, termHint: make(chan struct{}, 1), done: make(chan struct{})}
	l.active[id] = entry
	l.mu.Unlock()

	m := &liveEventMerge{l: l, id: id, runID: runID, ctx: c, entry: entry, dataWake: make(chan struct{}, 1), termHint: entry.termHint}
	// The broker subscription precedes the retained one: a publication of any
	// append after the retained initial drain is buffered as a wake.
	m.broker = s.broker.Subscribe(runID)
	if !s.Active(runID) {
		// Runs another process produces feed the alert manager only through
		// this ID-keyed tailer, which also republishes their appends as wakes.
		// A capture failure of the alert registration fails the subscribe
		// (typed): alerts disabled honestly, never a legacy degradation.
		rel, err := s.ensureEventSource(c, runID)
		if err != nil {
			m.cleanup()
			close(entry.done)
			return nil, closedCauseOr(c, err)
		}
		m.release = rel
	}
	// The registry lane: the manager's registration for this run, matched
	// against the subscription's binding; the Active-before-Register race
	// waits here (bounded by the subscription's context, F4).
	m.observed, m.observedObs = s.observedAlertLane(c, runID, true)
	inner, err := l.files.SubscribeEventsHinted(c, runID, position, runstream.FileStreamHints{Drain: m.dataWake, Terminal: m.termHint, TerminalPoll: terminalPoll})
	if err != nil {
		m.cleanup()
		close(entry.done)
		return nil, closedCauseOr(c, err)
	}
	m.inner = inner
	m.out = runstream.NewEventPipeWithClose(func() {
		cancel(context.Canceled)
		<-entry.done
	})
	go func() {
		defer close(entry.done)
		defer m.out.Finish(m.cleanup)
		m.run()
	}()
	return m.out, nil
}

// closedCause returns ErrSourceClosed when the Service stopped the
// subscription, nil for any other or no cancellation.
func closedCause(ctx context.Context) error {
	if cause := context.Cause(ctx); errors.Is(cause, runstream.ErrSourceClosed) {
		return runstream.ErrSourceClosed
	}
	return nil
}

// closedCauseOr returns ErrSourceClosed when the Service stopped ctx, err
// otherwise: a stop that interrupts a subscription's setup is that stop's
// typed end, never the setup's own cancellation error.
func closedCauseOr(ctx context.Context, err error) error {
	if cause := closedCause(ctx); cause != nil {
		return cause
	}
	return err
}

// liveEventMerge is one client's stream: the only producer of out.
type liveEventMerge struct {
	l                  *liveEventStreams
	id                 uint64
	runID              string
	ctx                context.Context
	entry              *liveEventEntry
	inner              runstream.EventSubscription
	broker             *EventSubscription
	observed           *EventSubscription
	observedObs        store.RunObservation
	retired            []*EventSubscription
	release            func()
	dataWake, termHint chan struct{}
	out                runstream.EventPipe
}

// fetchAlertLane adopts the run's current registry lane for this merge,
// parking the previous one in the retired queue (its buffered frames stay
// readable until finish/cleanup, F14). One-shot: it never stalls the
// merge's select loop — the Active-before-Register race is waited out at
// subscribe time only.
func (m *liveEventMerge) fetchAlertLane() {
	// A lane still held is still live: adopting a second subscription onto
	// the same epoch here would double-deliver its frames (C F5). The lazy
	// adoption happens when the held lane closes (the park sets it nil).
	if m.observed != nil {
		return
	}
	sub, obs := m.l.svc.observedAlertLane(m.ctx, m.runID, false)
	if sub != nil {
		m.observed = sub
		m.observedObs = obs
	}
}

// shipAlerts drains one alert lane's buffered frames (best effort, bounded
// by what it holds). It never cancels: cleanup cancels the lanes, after the
// drains. Closed lanes keep buffered frames readable until then.
func (m *liveEventMerge) shipAlerts(sub *EventSubscription) bool {
	for n := len(sub.C); n > 0; n-- {
		ev, ok := <-sub.C
		if !ok {
			break
		}
		if ev.Type != store.EventAlert {
			continue
		}
		if !m.out.Ship(m.ctx, []*store.Event{ev}) {
			m.out.Fail(closedCause(m.ctx))
			return false
		}
	}
	return true
}

// admitAlert is the one admission of a broker-lane alert, applied by the live
// loop, the closed registry lane's drain (closing) and the end flush. A
// stamped frame is dropped as the registry lane's carrier duplicate — any
// stamped frame while the lane is held, only the closing lane's own
// observation at its drain — and, on a bound merge, when its header is not the
// binding's. An unstamped frame has no registry copy and ships (B-F2): runs
// nobody registered keep their ID-keyed carrier.
func (m *liveEventMerge) admitAlert(ev *store.Event, closing bool) bool {
	stamp, stamped := ev.EventObservation()
	if !stamped {
		return true
	}
	if m.observed != nil && (!closing || stamp == m.observedObs) {
		return false
	}
	if binding, bound := store.RunDataBindingFrom(m.ctx); bound && stamp.Header() != binding.Header() {
		return false
	}
	return true
}

// run merges the retained subscription with the broker lane until the
// retained subscription ends. Every cancellation ends it too (it shares this
// stream's context), so its end is the merge's only exit besides a refused
// delivery: there is no second exit a Service stop could race.
func (m *liveEventMerge) run() {
	events, errs := m.inner.Events(), m.inner.Errors()
	// The alert lane, read until the retained subscription reports a
	// permanent refusal: an alert after it may already be a successor's.
	alerts := m.broker.C
	var fatal error
	for {
		brokerLane := alerts
		if m.observed != nil && m.l.laneFirst != nil && m.l.laneFirst() {
			brokerLane = nil
		}
		select {
		case batch, ok := <-events:
			if !ok {
				m.finish(errs, fatal)
				return
			}
			// The retained subscription owns the persisted cursor.
			if !m.out.Ship(m.ctx, batch) {
				m.out.Fail(closedCause(m.ctx))
				return
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if runstream.IsPermanentError(err) {
				if fatal == nil {
					fatal = err
					alerts = nil
				}
				continue
			}
			m.out.Warn(err)
		case ev, ok := <-brokerLane:
			if !ok {
				// CloseRun: the in-process producer is done with this run ID
				// (or paused, which never closes it). Check the status now,
				// then keep an alert lane: a run still running on disk (an
				// orphaned runner) keeps streaming. The registry lane
				// re-arms one-shot: a resume that re-adopts the observation
				// installs a new epoch and the merge picks it up lazily on
				// its next admitted legacy alert.
				signal(m.termHint)
				m.broker = m.l.svc.broker.Subscribe(m.runID)
				alerts = m.broker.C
				m.fetchAlertLane()
				continue
			}
			if ev.Type != store.EventAlert {
				// A persisted event's publication: drain the file now.
				signal(m.dataWake)
				continue
			}
			// Alerts are out of band: never deduped, never advancing the cursor.
			// The registry lane carries the registration's own frames; while it
			// is live, the legacy carrier of the same alert is dropped (F5).
			// A bound merge admits a stamped frame only when its origin matches
			// the binding (R6); unstamped frames — runs nobody registered —
			// ship as they always did.
			if !m.admitAlert(ev, false) {
				continue
			}
			if !m.out.Ship(m.ctx, []*store.Event{ev}) {
				m.out.Fail(closedCause(m.ctx))
				return
			}
			// An admitted legacy alert is evidence a registration may now
			// exist (a resume re-adopted the observation): adopt its epoch
			// so later alerts dedup against the registry lane.
			m.fetchAlertLane()
		case ev, ok := <-m.observedAlerts():
			if !ok {
				// The registration's epoch ended: the buffered frames stay
				// readable — park the lane in the retired queue, deliverable
				// by finish (F14); no cancel yet. The legacy frames queued
				// while the lane was live are this lane's own alerts' carrier
				// duplicates: drain them under the lane's dedup BEFORE the
				// park opens admission — stamped frames dropped, unstamped
				// ones and wakes keeping today's route (H3: a frame
				// published after the release is a fresh dispatch, not a
				// duplicate).
				for n := len(alerts); n > 0; n-- {
					ev2, ok2 := <-alerts
					if !ok2 {
						m.broker = m.l.svc.broker.Subscribe(m.runID)
						alerts = m.broker.C
						break
					}
					if ev2.Type != store.EventAlert {
						signal(m.dataWake)
						continue
					}
					// Only the closing lane's own observation marks a carrier
					// duplicate here, and a RunObservation has no epoch: the
					// copy of an alert that a re-adoption of the same
					// observation published while this lane was closing is
					// dropped with them, and its registry frame went to the
					// new epoch before this merge adopted it — that alert
					// reaches this stream only as its persisted run_health
					// twin. A frame stamped with another origin is refused
					// by the binding.
					if !m.admitAlert(ev2, true) {
						continue
					}
					if !m.out.Ship(m.ctx, []*store.Event{ev2}) {
						m.out.Fail(closedCause(m.ctx))
						m.retired = append(m.retired, m.observed)
						m.observed = nil
						return
					}
				}
				m.retired = append(m.retired, m.observed)
				m.observed = nil
				continue
			}
			// Registry frames are one event class by construction (R2): no
			// cursor effect, no dedup.
			if !m.out.Ship(m.ctx, []*store.Event{ev}) {
				m.out.Fail(closedCause(m.ctx))
				return
			}
		}
	}
}

// observedAlerts returns the registry lane's channel, or nil when the merge
// holds none — a nil channel blocks forever in the select, which is exactly
// the "no lane" state.
func (m *liveEventMerge) observedAlerts() <-chan *store.Event {
	if m.observed == nil {
		return nil
	}
	return m.observed.C
}

// finish ends the stream once the retained subscription ended. A Service
// stop wins over any other outcome; a permanent refusal is published; a
// consumer that went away gets nothing more; a clean end first forwards the
// alerts already buffered on the broker lane (best effort: an alert published
// later by a detached sink is not awaited).
func (m *liveEventMerge) finish(errs <-chan error, fatal error) {
	if errs != nil {
		for err := range errs {
			if runstream.IsPermanentError(err) && fatal == nil {
				fatal = err
			}
		}
	}
	if hook := m.l.beforeFinish; hook != nil {
		hook(m.ctx)
	}
	if cause := closedCause(m.ctx); cause != nil {
		m.out.Fail(cause)
		return
	}
	if fatal != nil {
		// Buffered alerts may already be a successor's: none is forwarded.
		m.out.Fail(fatal)
		return
	}
	if m.ctx.Err() != nil {
		return
	}
	for n := len(m.broker.C); n > 0; n-- {
		ev, ok := <-m.broker.C
		if !ok {
			// The lane closed mid-drain: `break`, not `return` — the retired
			// and live lanes' buffered alerts still drain below (M3).
			break
		}
		if ev.Type != store.EventAlert {
			continue
		}
		// The same admission as the live loop: a registry lane still held
		// dedups the legacy carrier; a bound merge admits a stamped frame
		// only on a matching origin.
		if !m.admitAlert(ev, false) {
			continue
		}
		if !m.out.Ship(m.ctx, []*store.Event{ev}) {
			m.out.Fail(closedCause(m.ctx))
			return
		}
	}
	// The retired lanes first (the epoch that ended mid-stream, F14), then
	// the live one: buffered alerts drain before their cancels.
	for _, sub := range m.retired {
		if !m.shipAlerts(sub) {
			return
		}
	}
	if m.observed != nil {
		if !m.shipAlerts(m.observed) {
			return
		}
	}
}

// cleanup releases what the merge owns, the retained subscription joined
// first, then unregisters it.
func (m *liveEventMerge) cleanup() {
	m.entry.cancel(context.Canceled)
	if m.inner != nil {
		_ = m.inner.Close()
	}
	m.broker.Cancel()
	if m.observed != nil {
		m.observed.Cancel()
	}
	for _, sub := range m.retired {
		sub.Cancel()
	}
	if m.release != nil {
		m.release()
	}
	m.l.mu.Lock()
	delete(m.l.active, m.id)
	m.l.mu.Unlock()
}

// signal sends a level-triggered hint without blocking: one pending token
// covers every request made before the receiver consumes it.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// stop closes admission, cancels every registered stream with ErrSourceClosed
// — even when ctx has already expired — and joins them within ctx. The
// Service FileSource is closed only after every stream joined; on a deadline
// the unjoined streams and the source stay registered, and a later stop
// finishes the cleanup.
func (l *liveEventStreams) stop(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	l.closed = true
	entries := make([]*liveEventEntry, 0, len(l.active))
	for _, e := range l.active {
		entries = append(entries, e)
	}
	logs := make([]*liveLogEntry, 0, len(l.logActive)+len(l.logPending))
	for _, e := range l.logActive {
		logs = append(logs, e)
	}
	// A bound log subscription still retrying its admission is the
	// Service's too: cancelled and joined like a merge, it ends typed.
	for _, e := range l.logPending {
		logs = append(logs, e)
	}
	l.mu.Unlock()
	for _, e := range entries {
		e.cancel(runstream.ErrSourceClosed)
	}
	// A bound log merge is joined like an event stream: the run's bytes come
	// from the same FileSource, which only closes once every reader did.
	for _, e := range logs {
		e.cancel(runstream.ErrSourceClosed)
	}
	dones := make([]<-chan struct{}, 0, len(entries)+len(logs))
	for _, e := range entries {
		dones = append(dones, e.done)
	}
	for _, e := range logs {
		dones = append(dones, e.done)
	}
	for _, done := range dones {
		select {
		case <-done:
			continue
		default:
		}
		select {
		case <-done:
		case <-ctx.Done():
			// Prefer a completed join if cancellation raced the deadline.
			select {
			case <-done:
				continue
			default:
				return fmt.Errorf("runview: live event streams not joined: %w", ctx.Err())
			}
		}
	}
	// Every retained subscription was closed and joined by its merge: closing
	// the source waits for no worker.
	return l.files.Close()
}

// awaitRuns lets every stream of runIDs registered now end on its own with
// its run, within ctx and grace, before Drain stops the rest. Drain calls it
// after every drained handle ended and its final status is written — or once
// ctx gave out — and asks each stream to check that status at once: a run
// that never gets CloseRun (a subbot child), or got it while still running
// (a detached runner's exit), ends without waiting for a poll. A stream
// still open when the wait ends (a run that ended paused, or an expired ctx)
// is left to stop.
func (l *liveEventStreams) awaitRuns(ctx context.Context, runIDs map[string]bool, grace time.Duration) {
	if l == nil || len(runIDs) == 0 {
		return
	}
	l.mu.Lock()
	var pending []*liveEventEntry
	for _, e := range l.active {
		if runIDs[e.runID] {
			pending = append(pending, e)
		}
	}
	l.mu.Unlock()
	for _, e := range pending {
		signal(e.termHint)
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for _, e := range pending {
		select {
		case <-e.done:
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		}
	}
}

// stopLiveEvents ends the live event streams; a join deadline is reported,
// not hidden. The alert registrations release here too — after the streams
// ended, never before (F13): dispatches admitted before Stop keep their
// captured routes until the lanes close at this release.
func (s *Service) stopLiveEvents(ctx context.Context) {
	if err := s.liveEvents.stop(ctx); err != nil {
		// Streams still open: their merges may still deliver, so their
		// registrations stay until a later stop or the reap joins them
		// (codex-F5). Releasing here would strand those merges' lanes.
		s.logger.Warn("live event streams: shutdown cleanup not joined: %v", err)
		return
	}
	if s.alertManager != nil {
		s.alertManager.ReleaseAll()
	}
}

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
// subscription, which contributes only out-of-band alerts and wake hints.
// A broker payload other than an alert is never delivered.
//
// The Service owns the FileSource and every merge; the store is borrowed.
// stop cancels and joins them with ErrSourceClosed.
type liveEventStreams struct {
	svc   *Service
	files *runstream.FileSource

	mu     sync.Mutex
	closed bool
	next   uint64
	active map[uint64]*liveEventEntry

	// beforeFinish, when set (tests), runs on the merge goroutine once the
	// retained subscription has ended, before the merge decides how its own
	// stream ends.
	beforeFinish func(ctx context.Context)
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
		svc:    s,
		files:  runstream.NewFileSource(native, "", s.logger),
		active: make(map[uint64]*liveEventEntry),
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
		m.release = s.ensureEventSource(runID)
	}
	inner, err := l.files.SubscribeEventsHinted(c, runID, position, runstream.FileStreamHints{Drain: m.dataWake, Terminal: m.termHint, TerminalPoll: terminalPoll})
	if err != nil {
		if cause := closedCause(c); cause != nil {
			err = cause
		}
		m.cleanup()
		close(entry.done)
		return nil, err
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

// liveEventMerge is one client's stream: the only producer of out.
type liveEventMerge struct {
	l                  *liveEventStreams
	id                 uint64
	runID              string
	ctx                context.Context
	entry              *liveEventEntry
	inner              runstream.EventSubscription
	broker             *EventSubscription
	release            func()
	dataWake, termHint chan struct{}
	out                runstream.EventPipe
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
		case ev, ok := <-alerts:
			if !ok {
				// CloseRun: the in-process producer is done with this run ID
				// (or paused, which never closes it). Check the status now,
				// then keep an alert lane: a run still running on disk (an
				// orphaned runner) keeps streaming.
				signal(m.termHint)
				m.broker = m.l.svc.broker.Subscribe(m.runID)
				alerts = m.broker.C
				continue
			}
			if ev.Type != store.EventAlert {
				// A persisted event's publication: drain the file now.
				signal(m.dataWake)
				continue
			}
			// Alerts are out of band: never deduped, never advancing the cursor.
			if !m.out.Ship(m.ctx, []*store.Event{ev}) {
				m.out.Fail(closedCause(m.ctx))
				return
			}
		}
	}
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
			return
		}
		if ev.Type != store.EventAlert {
			continue
		}
		if !m.out.Ship(m.ctx, []*store.Event{ev}) {
			m.out.Fail(closedCause(m.ctx))
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
	l.mu.Unlock()
	for _, e := range entries {
		e.cancel(runstream.ErrSourceClosed)
	}
	for _, e := range entries {
		select {
		case <-e.done:
			continue
		default:
		}
		select {
		case <-e.done:
		case <-ctx.Done():
			// Prefer a completed join if cancellation raced the deadline.
			select {
			case <-e.done:
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
// not hidden.
func (s *Service) stopLiveEvents(ctx context.Context) {
	if err := s.liveEvents.stop(ctx); err != nil {
		s.logger.Warn("live event streams: shutdown cleanup not joined: %v", err)
	}
}

package runview

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// svcEventTerminalPollInterval bounds how often an event subscription
// re-reads run.json to detect a terminal status. Runs produced in THIS
// process close their broker channel via broker.CloseRun, which triggers the
// check at once; runs produced elsewhere (external `iterion run`,
// dispatcher-spawned) never do, so without this poll the live tail would
// block forever on a finished run and the WS would never emit
// `terminated`. The FS twin of MongoSource's runIsTerminal poll. A var so
// tests can tighten it.
var svcEventTerminalPollInterval = 5 * time.Second

// This file exposes the ADR-053 streaming seam on the Service: one
// runstream.Source for the primary store, whatever the mode. Cloud mode
// delegates to the injected change-stream source. On the native filesystem
// store, events are retained per-client replay (service_live_events.go): each
// subscription reads the run it was opened on from disk, and the broker
// contributes only alerts and wake hints. Logs, and events of any other
// store, use the Service's own fan-out machinery (EventBroker +
// RunLogBuffer, fed directly by in-process runs and by the on-demand fsnotify
// tailers for everything else) behind the same contract, so the WS layer
// never branches on how a run was produced.

// StreamSource returns the store-agnostic streaming source for the
// primary store: the injected cloud source when one was wired
// (WithStreamSource — a construction-time fact), the Service's own
// broker/buffer-backed filesystem source otherwise.
func (s *Service) StreamSource() runstream.Source {
	if s.streamSrc != nil {
		return s.streamSrc
	}
	return &svcSource{s: s}
}

// svcSource is the filesystem-store source: the in-process
// EventBroker / RunLogBuffer fan-out behind the runstream contract.
type svcSource struct{ s *Service }

// Close is a no-op: the underlying machinery (broker, buffers) is owned
// by the Service lifecycle, not by this handle.
func (v *svcSource) Close() error { return nil }

// CheckBoundObservationSource admits this handle itself, over a store that
// endorses itself (store.RequireBoundObservationStore): its store reads —
// replay, terminal catch-up, log admission and persisted log ranges — run
// under the subscription context, and their permanent refusals end the
// subscription through Fail. A store that may drop that context is refused
// here, before any subscription. On the native filesystem store, persisted
// events are read from the bound run's retained selection only; alerts, log
// buffers and tailers, and the events of other stores after admission, stay
// routed by run ID.
func (v *svcSource) CheckBoundObservationSource(actual runstream.Source) error {
	receiver, ok := actual.(*svcSource)
	if v == nil || v.s == nil || !ok || receiver != v {
		return fmt.Errorf("%w: stream source %T is not this service source", store.ErrRunDataCapability, actual)
	}
	return store.RequireBoundObservationStore(v.s.store)
}

// SubscribeEvents delivers seq >= fromSeq. On the native filesystem store it
// is a retained per-client replay (liveEventStreams). Otherwise: paginated
// store replay first, then the live broker tail, deduped against the replay
// high-water mark. The broker subscription is opened BEFORE the replay so no
// event can fall between the two phases; the overlap is dropped by seq. Alert
// events (store.EventAlert, Seq==0, never persisted) bypass the dedup on both
// paths — they only ever arrive on the live tail.
func (v *svcSource) SubscribeEvents(ctx context.Context, runID string, fromSeq int64) (runstream.EventSubscription, error) {
	return v.SubscribeEventsCursor(ctx, runID, runstream.EventCursor{Seq: fromSeq})
}
func (v *svcSource) SubscribeEventsCursor(ctx context.Context, runID string, position runstream.EventCursor) (runstream.EventSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := v.s
	if s.liveEvents != nil {
		return s.liveEvents.subscribe(ctx, runID, position)
	}
	// Read once, by the subscriber: the producer goroutine never reads the
	// package variable a test may tighten after this subscription began.
	terminalPoll := svcEventTerminalPollInterval

	brokerSub := s.broker.Subscribe(runID)
	// Runs not produced in this process (external `iterion run`,
	// dispatcher-spawned) write events to disk but never publish to this
	// broker — bridge events.jsonl → broker for them. In-process runs
	// already feed the broker via the runtime observer.
	var release func()
	if !s.Active(runID) {
		// A capture failure of the alert registration fails the subscribe
		// (typed): alerts disabled honestly, never a legacy degradation.
		rel, err := s.ensureEventSource(ctx, runID)
		if err != nil {
			// The broker subscription opened above is this call's, and a
			// refused subscribe must not leave it in the broker's table
			// until CloseRun — a legacy-path run never gets one (B-F4).
			brokerSub.Cancel()
			return nil, err
		}
		release = rel
	}

	sub := runstream.NewEventPipe()
	cleanup := func() {
		brokerSub.Cancel()
		if release != nil {
			release()
		}
	}

	go func() {
		defer sub.Finish(cleanup)

		// Phase 1 — paginated replay of the persisted backlog. A partial
		// page ships before a permanent refusal surfaces: corruption, or a
		// run that is no longer the one the subscription context binds
		// (replaced, deleted, missing). Such a refusal is fatal — no live
		// tail follows it. Other load errors end the replay with a warning
		// and the live tail still runs (the disk history may be unreadable
		// while the broker still works).
		progress, err := runstream.ReplayEventsCursor(ctx, s.store, runID, position, sub)
		position = progress.Cursor
		if runstream.IsPermanentError(err) {
			sub.Fail(err)
			return
		}
		if err != nil && ctx.Err() == nil {
			sub.Warn(err)
		}

		// External/dispatcher runs (release != nil, i.e. !s.Active) never
		// get a broker CloseRun — the in-process observer that would call
		// it lives in another process. Without a terminal signal the tail
		// below blocks forever on a finished run and the WS never emits
		// `terminated`. Poll run.json and, on a terminal status (already
		// terminal at subscribe time, or a mid-stream flip), do a final
		// store replay to flush any events the tailer bridge hasn't
		// delivered yet, then end the stream. In-process runs (release ==
		// nil) close via broker.CloseRun as before.
		external := release != nil
		if external {
			next, done := s.eventTerminalCatchUp(ctx, runID, position, sub)
			if done {
				return
			}
			position = next
		}
		var termC <-chan time.Time
		if external {
			tk := time.NewTicker(terminalPoll)
			defer tk.Stop()
			termC = tk.C
		}

		// Phase 2 — live broker tail. For in-process runs the channel
		// closes at run completion (broker.CloseRun); for external runs
		// the terminal poll above ends it.
		for {
			select {
			case <-ctx.Done():
				return
			case <-sub.Done():
				return
			case ev, ok := <-brokerSub.C:
				if !ok {
					return
				}
				if ev.Type == store.EventAlert {
					// Stamped-origin admission (R6): a bound merge without a
					// registry lane (a wrapper store) admits a stamped alert
					// only when its origin matches the binding; unstamped
					// frames — runs nobody registered — ship as they always
					// did.
					if binding, bound := store.RunDataBindingFrom(ctx); bound {
						if stamp, stamped := ev.EventObservation(); stamped && stamp.Header() != binding.Header() {
							continue
						}
					}
				} else if !position.Contains(ev.Seq) {
					continue
				}
				if !sub.Ship(ctx, []*store.Event{ev}) {
					return
				}
				if position.Contains(ev.Seq) {
					position = runstream.EventCursor{Seq: ev.Seq, After: true}
				}
			case <-termC:
				next, done := s.eventTerminalCatchUp(ctx, runID, position, sub)
				if done {
					return
				}
				position = next
			}
		}
	}()

	return sub, nil
}

// eventTerminalCatchUp reports whether runID has reached a terminal
// status; when it has, it replays events persisted after the current position
// (stragglers the live broker bridge may not have delivered yet) so the
// terminal event is never lost before the stream closes. Returns the
// advanced cursor and whether the subscription is over (done=true → the
// caller ends it, which the WS turns into `terminated`).
//
// Both reads run under the subscription context, so a bound subscription
// reads the run it was opened on, never a successor recreated under the same
// ID. A permanent refusal of either read fails the subscription (done=true
// after Fail). A transient error of either read is a warning and leaves it
// tailing: the next poll checks again and replays from the returned cursor,
// which counts what a cut-short final replay already delivered, so the
// terminal events are never dropped by a transient failure.
func (s *Service) eventTerminalCatchUp(ctx context.Context, runID string, position runstream.EventCursor, sub runstream.EventPipe) (runstream.EventCursor, bool) {
	run, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		if runstream.IsPermanentError(err) {
			sub.Fail(err)
			return position, true
		}
		if ctx.Err() == nil {
			sub.Warn(fmt.Errorf("runview: terminal check of %s: %w", runID, err))
		}
		return position, false
	}
	if !run.Status.IsTerminal() {
		return position, false
	}
	d, rerr := runstream.ReplayEventsCursor(ctx, s.store, runID, position, sub)
	switch {
	case rerr == nil:
		return d.Cursor, true
	case runstream.IsPermanentError(rerr):
		sub.Fail(rerr)
		return d.Cursor, true
	case ctx.Err() != nil:
		return d.Cursor, true
	default:
		sub.Warn(fmt.Errorf("runview: final replay of %s: %w", runID, rerr))
		return d.Cursor, false
	}
}

// SubscribeLogs delivers log bytes from fromOffset.
//
// An UNBOUND subscription keeps the legacy carrier (arbitration C): the
// in-process live buffer; an on-demand run.log tailer for an active run this
// process didn't launch; a one-shot replay of the persisted run.log for a
// terminal run (stream closes right after — a missing file just closes
// immediately).
//
// A context bound to an original run (store.BindRunObservation) is admitted
// first: the run is loaded under it, and a permanent refusal — the run
// replaced, deleted or missing, or its metadata not decoding — is returned
// instead of a subscription. A transient load error (the same classification
// as the event path) does not refuse a legitimate subscription: the
// subscription is returned and its producer retries the admission, publishing
// each failure as a warning, and reads nothing before the run is admitted. On
// the native filesystem store that retry is the Service's: StopBackground and
// Drain end it with runstream.ErrSourceClosed and join it.
// Both the synchronous and the retried admission then continue in
// serveBoundLogs — the ONE producer of a bound subscription, which reads the
// run's bytes from the store's retained selection and never from the ring.
func (v *svcSource) SubscribeLogs(ctx context.Context, runID string, fromOffset int64) (runstream.LogSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := v.s

	if _, bound := store.RunDataBindingFrom(ctx); bound {
		run, err := s.loadLogAdmission(ctx, runID)
		switch {
		case err == nil:
		case runstream.IsPermanentError(err) || ctx.Err() != nil:
			return nil, err
		default:
			sub := runstream.NewLogPipe()
			if l := s.liveEvents; l != nil {
				// On the native store the retry is the Service's: a stop
				// cancels it with ErrSourceClosed and joins it. The merge it
				// admits inherits p.ctx, which no success cancels;
				// pendingLogAdmission says how a stop reaches that merge.
				p, perr := l.registerPendingLog(ctx, runID)
				if perr != nil {
					return nil, perr
				}
				go func() {
					defer p.end()
					run, ok := s.retryLogAdmission(p.ctx, runID, sub, err)
					if !ok {
						sub.Fail(closedCause(p.ctx))
						sub.Finish(nil)
						p.cancel(context.Canceled)
						return
					}
					if hook := l.afterLogAdmission; hook != nil {
						hook(p.ctx)
					}
					s.serveBoundLogs(p.ctx, runID, fromOffset, run, sub)
				}()
				return sub, nil
			}
			go func() {
				run, ok := s.retryLogAdmission(ctx, runID, sub, err)
				if !ok {
					sub.Finish(nil)
					return
				}
				s.serveBoundLogs(ctx, runID, fromOffset, run, sub)
			}()
			return sub, nil
		}
		sub := runstream.NewLogPipe()
		go s.serveBoundLogs(ctx, runID, fromOffset, run, sub)
		return sub, nil
	}

	plan := s.planLogs(ctx, runID, nil)
	sub := runstream.NewLogPipe()
	go s.produceLogs(ctx, runID, fromOffset, plan, sub)
	return sub, nil
}

// loadLogAdmission is the bound log admission's one store read. The test hook
// (nil in production) fails it once so a witness can drive the retry against
// the native store's EXACT seam — never a wrapper, which stays legacy.
func (s *Service) loadLogAdmission(ctx context.Context, runID string) (*store.Run, error) {
	if s.logAdmissionHook != nil {
		if err := s.logAdmissionHook(ctx, runID); err != nil {
			return nil, err
		}
	}
	return s.store.LoadRun(ctx, runID)
}

// serveBoundLogs is the ONE producer of an admitted bound log subscription:
// the synchronous admission and the retried one both continue here, so no
// bound stream ever reads the legacy ring or stands up a tailer. It always
// closes out.
func (s *Service) serveBoundLogs(ctx context.Context, runID string, from int64, run *store.Run, out runstream.LogPipe) {
	if s.liveEvents != nil {
		// The native lane path: the bytes are the store's, the wakes the
		// producer's lane. subscribeLogs owns out once its merge started;
		// a refusal before that leaves the pipe to this caller — typed
		// ErrSourceClosed when a stop already cancelled ctx, a pending
		// admission's context.
		if err := s.liveEvents.subscribeLogs(ctx, runID, from, out); err != nil {
			out.Fail(closedCauseOr(ctx, err))
			out.Finish(nil)
		}
		return
	}
	// A store without the native live seams — a wrapper of the native store,
	// a cloud store wired without a bound source — keeps the legacy carrier.
	plan := s.planLogs(ctx, runID, run)
	s.produceLogs(ctx, runID, from, plan, out)
}

// logAdmissionRetryMax caps the delay between two admission attempts of a
// bound log subscription whose run could not be loaded.
const logAdmissionRetryMax = 5 * time.Second

// retryLogAdmission loads runID under the bound ctx until it is admitted
// (ok), refused permanently (Fail, not ok) or the subscription ends (not
// ok). Each transient failure, first included, is published as a warning.
func (s *Service) retryLogAdmission(ctx context.Context, runID string, sub runstream.LogPipe, first error) (*store.Run, bool) {
	err := first
	delay := 250 * time.Millisecond
	for {
		sub.Warn(fmt.Errorf("runview: log admission of %s: %w", runID, err))
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, false
		case <-sub.Done():
			t.Stop()
			return nil, false
		case <-t.C:
		}
		if delay *= 2; delay > logAdmissionRetryMax {
			delay = logAdmissionRetryMax
		}
		var run *store.Run
		run, err = s.loadLogAdmission(ctx, runID)
		switch {
		case err == nil:
			return run, true
		case runstream.IsPermanentError(err):
			sub.Fail(err)
			return nil, false
		case ctx.Err() != nil:
			return nil, false
		}
	}
}

// logPlan is what a log subscription reads: the run's live buffer, with
// the subscription opened on it and the release of an on-demand tailer, or
// none (buf nil) for a one-shot read of the persisted log.
type logPlan struct {
	buf     *RunLogBuffer
	logSub  *RunLogSubscription
	release func()
}

// planLogs picks the log source for runID; admitted is the run a bound
// subscription was admitted against, nil for an unbound one.
func (s *Service) planLogs(ctx context.Context, runID string, admitted *store.Run) logPlan {
	buf := s.GetLogBuffer(runID)
	var release func()
	if buf == nil && !s.Active(runID) {
		// No live buffer AND not produced in this process. If the run is
		// still active, stand up the on-demand tailer (refcounted); a
		// terminal run falls through to the one-shot replay.
		run := admitted
		if run == nil {
			run, _ = s.store.LoadRun(ctx, runID)
		}
		if run != nil && !run.Status.IsTerminal() {
			release, buf = s.ensureLogSource(ctx, runID)
		}
	}
	if buf == nil {
		return logPlan{}
	}
	// The buffer is keyed by run ID alone: a bound subscription reads it
	// only when it was created for the admitted run — a predecessor's
	// buffer a subscription still holds is not the admitted run's log. It
	// reads the persisted log instead, under its binding, without live
	// tailing while that buffer stays registered.
	if admitted != nil && !buf.BornOf(admitted.BirthID) {
		if release != nil {
			release()
		}
		return logPlan{}
	}
	// Subscribe BEFORE Snapshot so chunks landing during the read are
	// dedup'd by offset on our side rather than lost.
	return logPlan{buf: buf, logSub: buf.Subscribe(), release: release}
}

// produceLogs is the producer of a log subscription: it delivers the plan
// into sub and finishes it.
func (s *Service) produceLogs(ctx context.Context, runID string, fromOffset int64, plan logPlan, sub runstream.LogPipe) {
	if plan.buf == nil {
		defer sub.Finish(nil)
		data, err := s.readPersistedLogRange(ctx, runID, fromOffset, 0)
		if runstream.IsPermanentError(err) {
			sub.Fail(err)
			return
		}
		if err != nil && ctx.Err() == nil {
			sub.Warn(err)
		}
		if len(data) > 0 {
			sub.Ship(ctx, runstream.LogChunk{Offset: fromOffset, Data: data})
		}
		return
	}

	buf, logSub := plan.buf, plan.logSub
	defer sub.Finish(func() {
		logSub.Cancel()
		if plan.release != nil {
			plan.release()
		}
	})
	startOffset, snapshot, _ := buf.Snapshot(fromOffset)

	// The ring is a bounded tail; on long runs the early bytes are
	// evicted. Fill [fromOffset, startOffset) from the persisted
	// run.log, the authoritative source. A missing file or a transient
	// read error degrades to the ring's window; a permanent refusal
	// (the run is no longer the bound original) fails the stream.
	if startOffset > fromOffset {
		data, err := s.readPersistedLogRange(ctx, runID, fromOffset, startOffset)
		if runstream.IsPermanentError(err) {
			sub.Fail(err)
			return
		}
		if err != nil && ctx.Err() == nil {
			sub.Warn(err)
		}
		if len(data) > 0 {
			if !sub.Ship(ctx, runstream.LogChunk{Offset: fromOffset, Data: data}) {
				return
			}
		}
	}

	cutoff := startOffset + int64(len(snapshot))
	if len(snapshot) > 0 {
		if !sub.Ship(ctx, runstream.LogChunk{Offset: startOffset, Data: snapshot}) {
			return
		}
	}

	// Live tail deduped/sliced against the cutoff so bytes never go
	// out twice (chunk.Bytes is already a per-subscriber copy).
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Done():
			return
		case chunk, ok := <-logSub.C:
			if !ok {
				// Buffer closed (run completed / tailer released) —
				// the log stream is over.
				return
			}
			var shipped bool
			if cutoff, shipped = runstream.ShipLogChunk(ctx, sub, chunk.Offset, chunk.Bytes, cutoff); !shipped {
				return
			}
		}
	}
}

// readPersistedLogRange reads bytes [from, until) of the run's
// persisted log through the store's RunLogStore (the canonical run.log
// reader) under the subscription context. A store without log persistence
// yields nothing; a read error is returned for the caller to classify.
func (s *Service) readPersistedLogRange(ctx context.Context, runID string, from, until int64) ([]byte, error) {
	ls := store.AsRunLogStore(s.store)
	if ls == nil {
		return nil, nil
	}
	data, err := ls.ReadRunLogRange(ctx, runID, from, until)
	if err != nil {
		return nil, fmt.Errorf("runview: read persisted log %s [%d,%d): %w", runID, from, until, err)
	}
	return data, nil
}

package runview

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// svcEventTerminalPollInterval bounds how often an external/dispatcher
// run's event subscription re-reads run.json to detect a terminal
// status. Runs produced in THIS process close their broker channel via
// broker.CloseRun; runs produced elsewhere (external `iterion run`,
// dispatcher-spawned) never do, so without this poll the live tail would
// block forever on a finished run and the WS would never emit
// `terminated`. The FS twin of MongoSource's runIsTerminal poll and
// FileSource.tailUntilTerminal. A var so tests can tighten it.
var svcEventTerminalPollInterval = 5 * time.Second

// This file exposes the ADR-053 streaming seam on the Service: one
// runstream.Source for the primary store, whatever the mode. Cloud mode
// delegates to the injected change-stream source; local mode wraps the
// Service's own fan-out machinery (EventBroker + RunLogBuffer, fed
// directly by in-process runs and by the on-demand fsnotify tailers for
// everything else) behind the same contract, so the WS layer never
// branches on how a run was produced.

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

// CheckBoundObservationSource admits this handle itself: its store reads —
// replay, terminal catch-up, log admission and persisted log ranges — run
// under the subscription context, and their permanent refusals end the
// subscription through Fail. What arrives after admission on the
// process-local broker, log buffers and alerts stays routed by run ID.
func (v *svcSource) CheckBoundObservationSource(actual runstream.Source) error {
	receiver, ok := actual.(*svcSource)
	if v == nil || v.s == nil || !ok || receiver != v {
		return fmt.Errorf("%w: stream source %T is not this service source", store.ErrRunDataCapability, actual)
	}
	return nil
}

// SubscribeEvents delivers seq >= fromSeq: paginated store replay first,
// then the live broker tail, deduped against the replay high-water mark.
// The broker subscription is opened BEFORE the replay so no event can
// fall between the two phases; the overlap is dropped by seq. Alert
// events (store.EventAlert, Seq==0, never persisted) bypass the dedup —
// they only ever arrive on the live tail.
func (v *svcSource) SubscribeEvents(ctx context.Context, runID string, fromSeq int64) (runstream.EventSubscription, error) {
	return v.SubscribeEventsCursor(ctx, runID, runstream.EventCursor{Seq: fromSeq})
}
func (v *svcSource) SubscribeEventsCursor(ctx context.Context, runID string, position runstream.EventCursor) (runstream.EventSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := v.s
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
		release = s.ensureEventSource(runID)
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
				if ev.Type != store.EventAlert && !position.Contains(ev.Seq) {
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

// SubscribeLogs delivers log bytes from fromOffset. Local resolution
// order (mirrors the historical handleSubscribeLogs cascade, now behind
// the seam): the in-process live buffer; an on-demand run.log tailer for
// an active run this process didn't launch; a one-shot replay of the
// persisted run.log for a terminal run (stream closes right after — a
// missing file just closes immediately).
//
// A context bound to an original run (store.BindRunObservation) is admitted
// first: the run is loaded under it before any buffer or tailer is looked
// up, because those are keyed by run ID alone. A permanent refusal — the
// run replaced, deleted or missing, or its metadata not decoding — is
// returned instead of a subscription.
// A transient load error (the same classification as the event path) does
// not refuse a legitimate subscription: the subscription is returned and its
// producer retries the admission, publishing each failure as a warning, and
// reads nothing before the run is admitted. Admission is a check at
// subscribe time: bytes a buffer delivers afterwards are not re-checked.
func (v *svcSource) SubscribeLogs(ctx context.Context, runID string, fromOffset int64) (runstream.LogSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := v.s

	var admitted *store.Run
	if _, bound := store.RunDataBindingFrom(ctx); bound {
		run, err := s.store.LoadRun(ctx, runID)
		switch {
		case err == nil:
			admitted = run
		case runstream.IsPermanentError(err) || ctx.Err() != nil:
			return nil, err
		default:
			sub := runstream.NewLogPipe()
			go func() {
				run, ok := s.retryLogAdmission(ctx, runID, sub, err)
				if !ok {
					sub.Finish(nil)
					return
				}
				plan := s.planLogs(ctx, runID, run)
				s.produceLogs(ctx, runID, fromOffset, plan, sub)
			}()
			return sub, nil
		}
	}

	plan := s.planLogs(ctx, runID, admitted)
	sub := runstream.NewLogPipe()
	go s.produceLogs(ctx, runID, fromOffset, plan, sub)
	return sub, nil
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
		run, err = s.store.LoadRun(ctx, runID)
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

package runview

import (
	"context"
	"errors"
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

// SubscribeEvents delivers seq >= fromSeq: paginated store replay first,
// then the live broker tail, deduped against the replay high-water mark.
// The broker subscription is opened BEFORE the replay so no event can
// fall between the two phases; the overlap is dropped by seq. Alert
// events (store.EventAlert, Seq==0, never persisted) bypass the dedup —
// they only ever arrive on the live tail.
func (v *svcSource) SubscribeEvents(ctx context.Context, runID string, fromSeq int64) (runstream.EventSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := v.s

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
		// page ships before a corruption error surfaces (fatal — no live
		// tail on a corrupt log); other load errors end the replay but
		// the live tail still runs (the disk history may be unreadable
		// while the broker still works).
		maxReplayed, err := runstream.ReplayEvents(ctx, s.store, runID, fromSeq, sub)
		if errors.Is(err, store.ErrEventsCorrupted) {
			sub.Warn(err)
			return
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
			if _, done := s.eventTerminalCatchUp(ctx, runID, maxReplayed, sub); done {
				return
			}
		}
		var termC <-chan time.Time
		if external {
			tk := time.NewTicker(svcEventTerminalPollInterval)
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
				if ev.Type != store.EventAlert && ev.Seq <= maxReplayed {
					continue
				}
				if !sub.Ship(ctx, []*store.Event{ev}) {
					return
				}
				if ev.Seq > maxReplayed {
					maxReplayed = ev.Seq
				}
			case <-termC:
				if _, done := s.eventTerminalCatchUp(ctx, runID, maxReplayed, sub); done {
					return
				}
			}
		}
	}()

	return sub, nil
}

// eventTerminalCatchUp reports whether runID has reached a terminal
// status; when it has, it replays events persisted past maxReplayed
// (stragglers the live broker bridge may not have delivered yet) so the
// terminal event is never lost before the stream closes. Returns the
// advanced high-water seq and whether the run is terminal (done=true →
// the caller ends the subscription, which the WS turns into `terminated`).
func (s *Service) eventTerminalCatchUp(ctx context.Context, runID string, maxReplayed int64, sub runstream.EventPipe) (int64, bool) {
	run, err := s.LoadRun(runID)
	if err != nil || !run.Status.IsTerminal() {
		return maxReplayed, false
	}
	if d, rerr := runstream.ReplayEvents(ctx, s.store, runID, maxReplayed+1, sub); rerr == nil {
		maxReplayed = d
	}
	return maxReplayed, true
}

// SubscribeLogs delivers log bytes from fromOffset. Local resolution
// order (mirrors the historical handleSubscribeLogs cascade, now behind
// the seam): the in-process live buffer; an on-demand run.log tailer for
// an active run this process didn't launch; a one-shot replay of the
// persisted run.log for a terminal run (stream closes right after — a
// missing file just closes immediately).
func (v *svcSource) SubscribeLogs(ctx context.Context, runID string, fromOffset int64) (runstream.LogSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := v.s

	buf := s.GetLogBuffer(runID)
	var release func()
	if buf == nil && !s.Active(runID) {
		// No live buffer AND not produced in this process. If the run is
		// still active, stand up the on-demand tailer (refcounted); a
		// terminal run falls through to the one-shot replay below.
		if run, err := s.LoadRun(runID); err == nil && !run.Status.IsTerminal() {
			release, buf = s.ensureLogSource(runID)
		}
	}

	if buf == nil {
		sub := runstream.NewLogPipe()
		go func() {
			defer sub.Finish(nil)
			if data := s.readPersistedLogRange(ctx, runID, fromOffset, 0); len(data) > 0 {
				sub.Ship(ctx, runstream.LogChunk{Offset: fromOffset, Data: data})
			}
		}()
		return sub, nil
	}

	// Subscribe BEFORE Snapshot so chunks landing during the read are
	// dedup'd by offset on our side rather than lost.
	logSub := buf.Subscribe()
	sub := runstream.NewLogPipe()
	cleanup := func() {
		logSub.Cancel()
		if release != nil {
			release()
		}
	}

	go func() {
		defer sub.Finish(cleanup)

		startOffset, snapshot, _ := buf.Snapshot(fromOffset)

		// The ring is a bounded tail; on long runs the early bytes are
		// evicted. Fill [fromOffset, startOffset) from the persisted
		// run.log, the authoritative source. Best-effort: a missing file
		// just degrades to the ring's window.
		if startOffset > fromOffset {
			if data := s.readPersistedLogRange(ctx, runID, fromOffset, startOffset); len(data) > 0 {
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
	}()

	return sub, nil
}

// readPersistedLogRange reads bytes [from, until) of the run's
// persisted log through the store's RunLogStore (the canonical run.log
// reader). Best-effort: a store without log persistence or a read
// error yields nil (the caller degrades to the ring's window).
func (s *Service) readPersistedLogRange(ctx context.Context, runID string, from, until int64) []byte {
	ls := store.AsRunLogStore(s.store)
	if ls == nil {
		return nil
	}
	data, err := ls.ReadRunLogRange(ctx, runID, from, until)
	if err != nil {
		s.logger.Warn("runview: read persisted log %s [%d,%d): %v", runID, from, until, err)
		return nil
	}
	return data
}

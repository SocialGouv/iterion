package runview

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// This file exposes the ADR-053 streaming seam on the Service: one
// runstream.Source for the primary store, whatever the mode. Cloud mode
// delegates to the injected change-stream source; local mode wraps the
// Service's own fan-out machinery (EventBroker + RunLogBuffer, fed
// directly by in-process runs and by the on-demand fsnotify tailers for
// everything else) behind the same contract, so the WS layer never
// branches on how a run was produced.

// StreamSource returns the store-agnostic streaming source for the
// primary store. The returned value is cheap and stateless — callers
// may fetch it per connection.
func (s *Service) StreamSource() runstream.Source {
	return &svcSource{s: s}
}

type svcSource struct{ s *Service }

func (v *svcSource) Capabilities() runstream.Capabilities {
	if v.s.streamSrc != nil {
		return v.s.streamSrc.Capabilities()
	}
	return runstream.Capabilities{LiveTail: true, HistoricalRange: true, Logs: true}
}

// Close is a no-op: the underlying machinery (broker, buffers, injected
// cloud source) is owned by the Service lifecycle, not by this handle.
func (v *svcSource) Close() error { return nil }

// SubscribeEvents delivers seq >= fromSeq: paginated store replay first,
// then the live broker tail, deduped against the replay high-water mark.
// The broker subscription is opened BEFORE the replay so no event can
// fall between the two phases; the overlap is dropped by seq. Alert
// events (store.EventAlert, Seq==0, never persisted) bypass the dedup —
// they only ever arrive on the live tail.
func (v *svcSource) SubscribeEvents(ctx context.Context, runID string, fromSeq int64) (runstream.EventSubscription, error) {
	if v.s.streamSrc != nil {
		return v.s.streamSrc.SubscribeEvents(ctx, runID, fromSeq)
	}
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
		release = s.EnsureEventSource(runID)
	}

	sub := newFsSub[[]*store.Event]()
	cleanup := func() {
		brokerSub.Cancel()
		if release != nil {
			release()
		}
	}

	go func() {
		defer sub.finish(cleanup)

		// Phase 1 — paginated replay of the persisted backlog. A partial
		// page ships before a corruption error surfaces (fatal); other
		// load errors end the replay but the live tail still runs (the
		// disk history may be unreadable while the broker still works).
		maxReplayed := fromSeq - 1
		next := fromSeq
		for {
			page, err := s.store.LoadEventsRange(ctx, runID, next, 0, runstream.MaxEventsPerPage)
			if len(page) > 0 {
				if !sub.ship(ctx, page) {
					return
				}
				if last := page[len(page)-1].Seq; last > maxReplayed {
					maxReplayed = last
				}
			}
			if errors.Is(err, store.ErrEventsCorrupted) {
				sub.fatal(err)
				return
			}
			if err != nil || len(page) < runstream.MaxEventsPerPage {
				break
			}
			next = maxReplayed + 1
		}

		// Phase 2 — live broker tail. The channel closes at run
		// completion (broker.CloseRun), which closes this subscription;
		// the WS layer translates that into its terminated envelope.
		for {
			select {
			case <-ctx.Done():
				return
			case <-sub.done:
				return
			case ev, ok := <-brokerSub.C:
				if !ok {
					return
				}
				if ev.Type != store.EventAlert && ev.Seq <= maxReplayed {
					continue
				}
				if !sub.ship(ctx, []*store.Event{ev}) {
					return
				}
				if ev.Seq > maxReplayed {
					maxReplayed = ev.Seq
				}
			}
		}
	}()

	return fsEventSub{sub}, nil
}

// SubscribeLogs delivers log bytes from fromOffset. Local resolution
// order (mirrors the historical handleSubscribeLogs cascade, now behind
// the seam): the in-process live buffer; an on-demand run.log tailer for
// an active run this process didn't launch; a one-shot replay of the
// persisted run.log for a terminal run (stream closes right after — a
// missing file just closes immediately).
func (v *svcSource) SubscribeLogs(ctx context.Context, runID string, fromOffset int64) (runstream.LogSubscription, error) {
	if src := v.s.streamSrc; src != nil {
		if src.Capabilities().Logs {
			return src.SubscribeLogs(ctx, runID, fromOffset)
		}
		// Interim cloud behaviour until the run_logs pipeline lands:
		// no log stream will ever exist — close immediately so the
		// client renders its "no log captured" state.
		sub := newFsSub[runstream.LogChunk]()
		sub.finish(nil)
		return fsLogSub{sub}, nil
	}
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
			release, buf = s.EnsureLogSource(runID)
		}
	}

	if buf == nil {
		sub := newFsSub[runstream.LogChunk]()
		go func() {
			defer sub.finish(nil)
			data, total := s.readPersistedLogRange(runID, fromOffset, 0)
			if len(data) > 0 {
				sub.ship(ctx, runstream.LogChunk{Offset: fromOffset, Data: data, Total: total})
			}
		}()
		return fsLogSub{sub}, nil
	}

	// Subscribe BEFORE Snapshot so chunks landing during the read are
	// dedup'd by offset on our side rather than lost.
	logSub := buf.Subscribe()
	sub := newFsSub[runstream.LogChunk]()
	cleanup := func() {
		logSub.Cancel()
		if release != nil {
			release()
		}
	}

	go func() {
		defer sub.finish(cleanup)

		startOffset, snapshot, _ := buf.Snapshot(fromOffset)

		// The ring is a bounded tail; on long runs the early bytes are
		// evicted. Fill [fromOffset, startOffset) from the persisted
		// run.log, the authoritative source. Best-effort: a missing file
		// just degrades to the ring's window.
		if startOffset > fromOffset {
			if data, _ := s.readPersistedLogRange(runID, fromOffset, startOffset); len(data) > 0 {
				if !sub.ship(ctx, runstream.LogChunk{
					Offset: fromOffset,
					Data:   data,
					Total:  fromOffset + int64(len(data)),
				}) {
					return
				}
			}
		}

		cutoff := startOffset + int64(len(snapshot))
		if len(snapshot) > 0 {
			if !sub.ship(ctx, runstream.LogChunk{Offset: startOffset, Data: snapshot, Total: cutoff}) {
				return
			}
		}

		// Live tail with cutoff slicing so bytes never go out twice.
		for {
			select {
			case <-ctx.Done():
				return
			case <-sub.done:
				return
			case chunk, ok := <-logSub.C:
				if !ok {
					// Buffer closed (run completed / tailer released) —
					// the log stream is over.
					return
				}
				data := chunk.Bytes
				offset := chunk.Offset
				if offset < cutoff {
					skip := int(cutoff - offset)
					if skip >= len(data) {
						continue
					}
					data = data[skip:]
					offset = cutoff
				}
				if !sub.ship(ctx, runstream.LogChunk{
					Offset: offset,
					Data:   data,
					Total:  offset + int64(len(data)),
				}) {
					return
				}
				cutoff = offset + int64(len(data))
			}
		}
	}()

	return fsLogSub{sub}, nil
}

// readPersistedLogRange reads bytes [from, until) of the run's persisted
// run.log; until <= 0 means "to end of file". Returns the bytes plus the
// end offset of what was read (from + len). Best-effort: a missing or
// unreadable file returns nil.
func (s *Service) readPersistedLogRange(runID string, from, until int64) ([]byte, int64) {
	if s.storeDir == "" {
		return nil, from
	}
	if from < 0 {
		from = 0
	}
	logPath := filepath.Join(s.storeDir, "runs", runID, "run.log")
	f, err := os.Open(logPath)
	if err != nil {
		return nil, from
	}
	defer f.Close()

	if until <= 0 {
		st, err := f.Stat()
		if err != nil {
			return nil, from
		}
		until = st.Size()
	}
	if from >= until {
		return nil, from
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil, from
	}
	buf := make([]byte, until-from)
	n, _ := io.ReadFull(f, buf)
	if n == 0 {
		return nil, from
	}
	return buf[:n], from + int64(n)
}

// fsSub is the generic subscription handle shared by the local event
// and log paths: a delivery channel, an error channel, a done signal
// for Close, and once-guarded teardown.
type fsSub[T any] struct {
	ch     chan T
	errs   chan error
	done   chan struct{}
	closeO sync.Once  // user-facing Close
	finO   sync.Once  // channel close + cleanup (producer side)
	fatalO sync.Once  // at most one fatal error
}

func newFsSub[T any]() *fsSub[T] {
	return &fsSub[T]{
		ch:   make(chan T, 8),
		errs: make(chan error, 4),
		done: make(chan struct{}),
	}
}

// ship delivers one value, aborting on cancellation. Returns false when
// the subscription is over and the producer must stop.
func (s *fsSub[T]) ship(ctx context.Context, v T) bool {
	select {
	case s.ch <- v:
		return true
	case <-ctx.Done():
		return false
	case <-s.done:
		return false
	}
}

// fatal surfaces a terminal error; the producer must return right after
// (finish closes the channels, which is the "stream over" signal).
func (s *fsSub[T]) fatal(err error) {
	s.fatalO.Do(func() {
		select {
		case s.errs <- err:
		default:
		}
	})
}

// finish closes the channels and runs cleanup exactly once. Called by
// the producer goroutine on exit; also invoked via Close for producers
// that already returned.
func (s *fsSub[T]) finish(cleanup func()) {
	s.finO.Do(func() {
		if cleanup != nil {
			cleanup()
		}
		close(s.ch)
		close(s.errs)
	})
}

func (s *fsSub[T]) Close() error {
	s.closeO.Do(func() { close(s.done) })
	return nil
}

func (s *fsSub[T]) Errors() <-chan error { return s.errs }

// fsEventSub / fsLogSub adapt the generic handle to the runstream
// interfaces (Go can't declare methods on a generic instantiation, so
// each stream kind gets a thin named wrapper).
type fsEventSub struct{ *fsSub[[]*store.Event] }

func (s fsEventSub) Events() <-chan []*store.Event { return s.ch }

type fsLogSub struct{ *fsSub[runstream.LogChunk] }

func (s fsLogSub) Chunks() <-chan runstream.LogChunk { return s.ch }

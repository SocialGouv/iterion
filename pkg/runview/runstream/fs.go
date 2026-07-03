package runstream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// fileTerminalPollInterval bounds how often a FileSource subscription
// re-reads run.json to detect a terminal status. The foreign run is
// driven by a different daemon, so there is no in-process completion
// signal — without this poll the tail would run forever on a finished
// run (the file simply stops growing, indistinguishable from "still
// working" by file watch alone). A var so tests can tighten it.
var fileTerminalPollInterval = 5 * time.Second

// FileSource implements Source over a foreign filesystem store root —
// the cross-store observation mode (`?store=`): a run owned by another
// local daemon whose files this process can read but whose broker it
// cannot reach. Replay goes through the store's range API; the live
// tail rides the shared fsnotify tailers; termination is detected by
// polling run.json.
type FileSource struct {
	store  store.RunStore
	root   string
	logger *iterlog.Logger

	closeOnce sync.Once
	closed    chan struct{}
}

// NewFileSource builds a Source over the store rooted at root. st must
// be a RunStore opened on that same root (the caller typically has both
// from resolving the cross-store request).
func NewFileSource(st store.RunStore, root string, logger *iterlog.Logger) *FileSource {
	if logger == nil {
		logger = iterlog.New(iterlog.LevelInfo, nil)
	}
	return &FileSource{store: st, root: root, logger: logger, closed: make(chan struct{})}
}

// Close cancels every subscription spawned from this source.
func (f *FileSource) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

// SubscribeEvents replays the persisted backlog through the store's
// range API, then tails the foreign events.jsonl (deduping the overlap
// by seq) until the run reaches a terminal status, the subscription is
// closed, or ctx is cancelled. On terminal detection a final drain
// flushes the tail before the channel closes.
func (f *FileSource) SubscribeEvents(ctx context.Context, runID string, fromSeq int64) (EventSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pipe := NewEventPipe()

	go func() {
		defer pipe.Finish(nil)

		// Phase 1 — paginated replay. Load errors are non-fatal for a
		// foreign store (the file tail below is the authoritative
		// fallback); they just end the replay early.
		maxReplayed, err := ReplayEvents(ctx, f.store, runID, fromSeq, pipe)
		if err != nil {
			f.logger.Warn("runstream: cross-store replay (%s): %v", runID, err)
		}

		// Phase 2 — live file tail. lastSeq is only touched from the
		// tailer goroutine (after the replay above completed), so the
		// dedup needs no lock.
		lastSeq := maxReplayed
		emit := func(evt store.Event) {
			if evt.Seq <= lastSeq {
				return
			}
			e := evt
			if !pipe.Ship(ctx, []*store.Event{&e}) {
				return
			}
			lastSeq = evt.Seq
		}
		eventsPath := filepath.Join(f.root, "runs", runID, "events.jsonl")
		f.tailUntilTerminal(ctx, runID, pipe.Done(), func(done <-chan struct{}) {
			TailEventsFile(eventsPath, done, emit, f.logger)
		})
	}()

	return pipe, nil
}

// SubscribeLogs streams the foreign run.log from fromOffset. A missing
// file at subscribe time means it will never exist for this run (every
// producer that writes run.log creates it before the first event), so
// the stream closes immediately instead of polling forever.
func (f *FileSource) SubscribeLogs(ctx context.Context, runID string, fromOffset int64) (LogSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pipe := NewLogPipe()

	logPath := filepath.Join(f.root, "runs", runID, "run.log")
	if _, err := os.Stat(logPath); errors.Is(err, os.ErrNotExist) {
		pipe.Finish(nil)
		return pipe, nil
	}

	go func() {
		defer pipe.Finish(nil)

		// The tailer drains from byte 0; dedup/slice against the
		// delivered high-water mark so fromOffset semantics match every
		// other backend. The chunk aliases the tailer's scratch buffer
		// (TailLogFile's emit contract) — copy before shipping, since
		// the pipe retains it.
		delivered := fromOffset
		emit := func(off int64, chunk []byte) {
			if off+int64(len(chunk)) <= delivered {
				return
			}
			cp := make([]byte, len(chunk))
			copy(cp, chunk)
			delivered, _ = ShipLogChunk(ctx, pipe, off, cp, delivered)
		}
		f.tailUntilTerminal(ctx, runID, pipe.Done(), func(done <-chan struct{}) {
			TailLogFile(logPath, done, emit, f.logger)
		})
	}()

	return pipe, nil
}

// tailUntilTerminal runs tail (a blocking file tailer parametrised by
// its stop channel) until the run reaches a terminal status, the
// subscription closes, the source closes, or ctx ends. Closing the stop
// channel makes the tailer flush a final drain before returning, so the
// wait guarantees every byte/event persisted before termination was
// emitted.
func (f *FileSource) tailUntilTerminal(ctx context.Context, runID string, subDone <-chan struct{}, tail func(done <-chan struct{})) {
	tailDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tail(tailDone)
	}()
	stop := func() {
		close(tailDone)
		wg.Wait()
	}

	ticker := time.NewTicker(fileTerminalPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			stop()
			return
		case <-subDone:
			stop()
			return
		case <-f.closed:
			stop()
			return
		case <-ticker.C:
			run, err := f.store.LoadRun(ctx, runID)
			if err != nil {
				continue
			}
			if run.Status.IsTerminal() {
				stop() // final drain flushes anything written before the flip
				return
			}
		}
	}
}

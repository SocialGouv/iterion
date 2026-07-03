package runstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// This file holds the ONE filesystem tailer implementation every
// file-backed streaming path shares (ADR-053): the runview Service's
// detached/external-run sources and the cross-store FileSource. It was
// extracted from pkg/runview's file_event_source.go / file_log_source.go,
// which previously coexisted with a third copy inside pkg/server's
// cross-store WS handlers.
//
// Shared behaviours:
//   - fsnotify on the parent directory (so Create/rotate is seen), with
//     a 250 ms polling fallback when inotify is unavailable;
//   - a wide 10 s defensive re-drain for dropped fsnotify events;
//   - partial trailing lines / bytes are left for the next drain;
//   - truncation/rotation resets the offset to 0 and replays (log
//     consumers must treat a backwards-jumping offset as a re-anchor);
//   - a bounded wait for the file to first appear (the producer may not
//     have written anything when the subscriber connects).

// logChunkBudget caps each emitted log chunk to avoid pathological reads
// when the producer emits a megabyte burst.
const logChunkBudget = 64 * 1024

// tailPollFallbackInterval is the fsnotify-less polling cadence.
const tailPollFallbackInterval = 250 * time.Millisecond

// tailDefensivePollInterval is the wide safety-net re-drain that catches
// fsnotify events dropped on busy file systems.
const tailDefensivePollInterval = 10 * time.Second

// TailEventsFile tails an events.jsonl at path, invoking emit for every
// complete event line appended, until done closes (a final drain flushes
// tail bytes on exit). Malformed lines are logged and skipped. Blocking:
// callers run it on their own goroutine.
func TailEventsFile(path string, done <-chan struct{}, emit func(store.Event), logger *iterlog.Logger) {
	drain := func(offset int64) int64 {
		return drainNewEvents(path, offset, emit, logger)
	}
	tailFile(path, done, drain, logger)
}

// TailLogFile tails a run.log at path, invoking emit(offset, chunk) for
// every appended byte span (chunks capped at logChunkBudget), until done
// closes. On truncation/rotation the offset restarts at 0.
//
// The chunk slice is only valid for the duration of the emit call (it
// aliases the tailer's scratch buffer) — consumers that retain it past
// the call must copy. The hottest consumer, RunLogBuffer.Write, already
// copies internally; making the copy the retainer's job avoids a second
// full copy of every chunk on that path.
func TailLogFile(path string, done <-chan struct{}, emit func(offset int64, chunk []byte), logger *iterlog.Logger) {
	scratch := make([]byte, logChunkBudget)
	drain := func(offset int64) int64 {
		return drainNewLogBytes(path, offset, scratch, emit, logger)
	}
	tailFile(path, done, drain, logger)
}

// tailFile is the shared watch loop: wait for the file, prefer fsnotify,
// fall back to polling, re-drain defensively, final-drain on done.
func tailFile(path string, done <-chan struct{}, drain func(offset int64) int64, logger *iterlog.Logger) {
	// Wait (bounded) for the file to appear. A file still missing after
	// the window doesn't terminate us — the producer may be starting;
	// the watch loop below tolerates a missing file.
	waitForFile(path, done, 5*time.Second)

	watcher, watcherErr := fsnotify.NewWatcher()
	if watcherErr != nil {
		logger.Warn("runstream: tail %s: fsnotify unavailable, falling back to polling: %v", path, watcherErr)
		tailFilePolling(path, done, drain)
		return
	}
	defer watcher.Close()

	// Watch the directory rather than the file directly so we still see
	// Create events if the file is rotated or initially missing.
	dir := filepath.Dir(path)
	if err := watcher.Add(dir); err != nil {
		logger.Warn("runstream: tail %s: watcher.Add(%q): %v — falling back to polling", path, dir, err)
		tailFilePolling(path, done, drain)
		return
	}

	// Drain whatever already exists so the subscriber sees the full
	// backlog, not just bytes appended after subscription.
	offset := drain(0)

	pollTicker := time.NewTicker(tailDefensivePollInterval)
	defer pollTicker.Stop()

	// Snapshot the Errors channel so we can nil our reference if
	// fsnotify closes it — a closed channel is always ready to receive
	// and would spin this select.
	errs := watcher.Errors

	for {
		select {
		case <-done:
			drain(offset) // final drain to flush any tail bytes the watcher missed
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Clean(ev.Name) != filepath.Clean(path) {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				offset = drain(offset)
			}
		case <-pollTicker.C:
			offset = drain(offset)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			logger.Warn("runstream: tail %s: watcher error: %v", path, err)
		}
	}
}

// tailFilePolling is the fsnotify-less fallback. Slightly higher CPU but
// functionally equivalent.
func tailFilePolling(path string, done <-chan struct{}, drain func(offset int64) int64) {
	offset := drain(0)

	t := time.NewTicker(tailPollFallbackInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			drain(offset)
			return
		case <-t.C:
			offset = drain(offset)
		}
	}
}

// drainNewEvents reads any bytes appended past offset, parses them as
// one event per line, emits each, and returns the new offset. Partial
// trailing lines (write-in-progress) are left in the file and re-read on
// the next call when more bytes arrive.
func drainNewEvents(path string, offset int64, emit func(store.Event), logger *iterlog.Logger) int64 {
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Warn("runstream: event tail %s: open: %v", path, err)
		}
		return offset
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		// File rotated / truncated. Reset to start and re-read.
		logger.Warn("runstream: event tail %s: seek %d: %v — resetting to start", path, offset, err)
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return offset
		}
		offset = 0
	}

	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] != '\n' {
				// Partial line — don't consume it; re-read on next tick.
				return offset
			}
			offset += int64(len(line))
			trimmed := line[:len(line)-1]
			if len(trimmed) == 0 {
				continue
			}
			var evt store.Event
			if jerr := json.Unmarshal(trimmed, &evt); jerr != nil {
				logger.Warn("runstream: event tail %s: bad line at offset %d: %v", path, offset, jerr)
				continue
			}
			emit(evt)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				logger.Warn("runstream: event tail %s: read: %v", path, err)
			}
			return offset
		}
	}
}

// drainNewLogBytes reads any bytes appended past offset and emits them
// in chunks of at most logChunkBudget, tagged with their absolute file
// offset. Truncation (file shorter than offset) resets to 0 so the
// consumer re-anchors. Chunks alias the caller-owned scratch buffer —
// see TailLogFile's emit contract.
func drainNewLogBytes(path string, offset int64, scratch []byte, emit func(offset int64, chunk []byte), logger *iterlog.Logger) int64 {
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Warn("runstream: log tail %s: open: %v", path, err)
		}
		return offset
	}
	defer f.Close()

	if st, err := f.Stat(); err == nil && st.Size() < offset {
		offset = 0
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		logger.Warn("runstream: log tail %s: seek %d: %v — resetting to start", path, offset, err)
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return offset
		}
		offset = 0
	}

	for {
		n, readErr := f.Read(scratch)
		if n > 0 {
			emit(offset, scratch[:n])
			offset += int64(n)
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				logger.Warn("runstream: log tail %s: read: %v", path, readErr)
			}
			return offset
		}
		if n < len(scratch) {
			return offset
		}
	}
}

// waitForFile blocks until path exists, until done is closed, or until
// budget elapses — whichever comes first.
func waitForFile(path string, done <-chan struct{}, budget time.Duration) {
	deadline := time.Now().Add(budget)
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-done:
			return
		case <-t.C:
			if time.Now().After(deadline) {
				return
			}
		}
	}
}

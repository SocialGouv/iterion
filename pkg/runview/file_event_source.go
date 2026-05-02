package runview

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/SocialGouv/iterion/pkg/store"
)

// startEventSource spawns a goroutine that tails the run's
// events.jsonl using fsnotify and republishes each appended line via
// the service's EventBroker, so WS subscribers connected to the
// editor server receive events emitted by a detached runner
// subprocess (or by a runner that's outliving the server it was
// started by).
//
// The tailer terminates when the supplied done channel is closed —
// which spawnDetached arranges to happen when the runner subprocess
// exits.
//
// Failure modes:
//   - events.jsonl doesn't exist yet → poll the directory until it
//     appears (the runner may not have written its first event when
//     the subscriber connects).
//   - fsnotify unavailable on the platform / file system → fall back
//     to a 250 ms polling loop. fsnotify is the fast path; correctness
//     does not depend on it.
//   - malformed JSON line → log + skip. We don't want one corrupt
//     line to kill the entire stream.
func startEventSource(s *Service, runID string, done <-chan struct{}) {
	go func() {
		path := filepath.Join(s.storeDir, "runs", runID, "events.jsonl")
		tailEvents(s, runID, path, done)
	}()
}

// tailEvents is the long-running tail loop. It manages waiting for
// the file to appear, opening it, advancing through appended lines,
// and exiting cleanly on done.
func tailEvents(s *Service, runID, path string, done <-chan struct{}) {
	// Wait for the file to appear (the runner may not have written
	// its first event yet). Bounded poll: if it doesn't appear within
	// a reasonable window we still proceed — the watcher loop tolerates
	// a missing file and re-checks on every tick.
	if !waitForFile(path, done, 5*time.Second) {
		// File didn't appear in the bounded window, but that doesn't
		// terminate us — the runner may still be starting. Fall through
		// into the watch loop with the file potentially missing.
	}

	watcher, watcherErr := fsnotify.NewWatcher()
	if watcherErr != nil {
		s.logger.Warn("runview: event tail (%s): fsnotify unavailable, falling back to polling: %v", runID, watcherErr)
		tailEventsPolling(s, runID, path, done)
		return
	}
	defer watcher.Close()

	// Watch the directory rather than the file directly so we still
	// see Create events if events.jsonl is rotated or initially
	// missing.
	dir := filepath.Dir(path)
	if err := watcher.Add(dir); err != nil {
		s.logger.Warn("runview: event tail (%s): watcher.Add(%q): %v — falling back to polling", runID, dir, err)
		tailEventsPolling(s, runID, path, done)
		return
	}

	var offset int64
	// Drain whatever already exists so the WS subscriber sees the
	// full event log, not just events appended after subscription.
	offset = drainNewEvents(s, runID, path, offset)

	// Defensive periodic drain in case fsnotify drops an event on a
	// busy file system. Set wide so an idle run doesn't burn cycles
	// across many concurrent runs; fsnotify itself wakes us promptly
	// for the common path.
	pollTicker := time.NewTicker(10 * time.Second)
	defer pollTicker.Stop()

	for {
		select {
		case <-done:
			// Final drain to flush any tail bytes the watcher missed.
			drainNewEvents(s, runID, path, offset)
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Clean(ev.Name) != filepath.Clean(path) {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				offset = drainNewEvents(s, runID, path, offset)
			}
		case <-pollTicker.C:
			offset = drainNewEvents(s, runID, path, offset)
		case watcherErr := <-watcher.Errors:
			s.logger.Warn("runview: event tail (%s): watcher error: %v", runID, watcherErr)
		}
	}
}

// tailEventsPolling is the fsnotify-less fallback. Slightly higher CPU
// (250 ms wakeups) but functionally equivalent.
func tailEventsPolling(s *Service, runID, path string, done <-chan struct{}) {
	var offset int64
	offset = drainNewEvents(s, runID, path, offset)

	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-done:
			drainNewEvents(s, runID, path, offset)
			return
		case <-t.C:
			offset = drainNewEvents(s, runID, path, offset)
		}
	}
}

// drainNewEvents reads any bytes appended past `offset`, parses them
// as one event per line, publishes each via s.broker.Publish, and
// returns the new offset. Partial trailing lines (write-in-progress)
// are left in the file and re-read on the next call when more bytes
// arrive.
func drainNewEvents(s *Service, runID, path string, offset int64) int64 {
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.logger.Warn("runview: event tail (%s): open: %v", runID, err)
		}
		return offset
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		// File rotated / truncated. Reset to start and re-read.
		s.logger.Warn("runview: event tail (%s): seek %d: %v — resetting to start", runID, offset, err)
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
				// Partial line — don't consume it; we'll re-read on next tick.
				return offset
			}
			offset += int64(len(line))
			trimmed := line[:len(line)-1]
			if len(trimmed) == 0 {
				continue
			}
			var evt store.Event
			if jerr := json.Unmarshal(trimmed, &evt); jerr != nil {
				s.logger.Warn("runview: event tail (%s): bad line at offset %d: %v", runID, offset, jerr)
				continue
			}
			s.broker.Publish(evt)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.logger.Warn("runview: event tail (%s): read: %v", runID, err)
			}
			return offset
		}
	}
}

// waitForFile blocks until path exists, until done is closed, or
// until budget elapses — whichever comes first. Returns true when the
// file became visible.
func waitForFile(path string, done <-chan struct{}, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		select {
		case <-done:
			return false
		case <-t.C:
			if time.Now().After(deadline) {
				return false
			}
		}
	}
}

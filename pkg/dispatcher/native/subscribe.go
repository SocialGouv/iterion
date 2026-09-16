package native

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/SocialGouv/iterion/internal/fswatch"
	"github.com/fsnotify/fsnotify"
)

// eventTailer follows <root>/events.jsonl and invokes a listener for
// every Event appended after the tailer started.
//
// Tailing the persisted log is the only writer-agnostic way to observe
// issue-state transitions: every *native.Store instance (the server's
// NativeTrackerStore, the dispatcher daemon, the fresh one opened per
// run in runview's executor) appends to the same shared events.jsonl,
// so an in-memory per-instance callback would miss transitions made
// through a sibling instance. Starting at EOF means history is never
// replayed — only transitions that happen after Subscribe fan out.
type eventTailer struct {
	w    *fsnotify.Watcher
	stop chan struct{}
	done chan struct{}
}

// Subscribe starts a goroutine that tails events.jsonl and calls fn for
// each newly-appended Event (in file order, after the current EOF).
// Returns a cancel func that stops the tailer and releases the fsnotify
// resources. Returns a nil cancel + error when fsnotify is unavailable
// on the host (read-only / kernel-restricted environment); callers
// should log and continue with fan-out disabled — the same degradation
// the index watcher already accepts.
//
// fn runs on the tailer goroutine and must not block for long; offload
// slow work (store I/O) to the caller's own goroutine if needed.
func (s *Store) Subscribe(fn func(Event)) (func(), error) {
	w, err := fswatch.NewWatcher()
	if err != nil {
		return nil, err
	}
	// Watch the root directory, not the file itself: events.jsonl may
	// not exist yet (no transition recorded), and a directory watch
	// catches both its creation and subsequent appends.
	if err := w.Add(s.root); err != nil {
		_ = w.Close()
		return nil, err
	}
	eventsPath := filepath.Join(s.root, eventsFile)

	// Seek to current EOF so only future events are delivered.
	var offset int64
	if fi, statErr := os.Stat(eventsPath); statErr == nil {
		offset = fi.Size()
	}

	t := &eventTailer{w: w, stop: make(chan struct{}), done: make(chan struct{})}
	go t.loop(eventsPath, offset, fn)

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(t.stop)
			<-t.done
			_ = t.w.Close()
		})
	}
	return cancel, nil
}

func (t *eventTailer) loop(eventsPath string, offset int64, fn func(Event)) {
	defer close(t.done)
	for {
		select {
		case <-t.stop:
			return
		case ev, ok := <-t.w.Events:
			if !ok {
				return
			}
			if filepath.Clean(ev.Name) != eventsPath {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			offset = readNewEvents(eventsPath, offset, fn)
		case _, ok := <-t.w.Errors:
			if !ok {
				return
			}
			// Drop kernel queue-full signals; a missed notification just
			// delays delivery until the next append fires another event.
		}
	}
}

// readNewEvents reads complete JSON lines from eventsPath starting at
// offset, invokes fn for each parseable Event, and returns the advanced
// offset. The offset only moves past newline-terminated lines, so a
// partial trailing write (writer mid-append) is re-read on the next
// notification rather than parsed as a truncated record.
func readNewEvents(eventsPath string, offset int64, fn func(Event)) int64 {
	f, err := os.Open(eventsPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0
		}
		return offset
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset
	}
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if err == nil && len(line) > 0 {
			offset += int64(len(line))
			var e Event
			if json.Unmarshal(line, &e) == nil {
				fn(e)
			}
			continue
		}
		// EOF (possibly with a partial trailing line): stop without
		// consuming the incomplete bytes.
		break
	}
	return offset
}

package native

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// indexWatcher watches <root>/issues/ for filesystem changes made by
// out-of-process writers (typically the `iterion __mcp-board` stdio
// MCP subprocess that whats-next's emit_action spawns) and refreshes
// the parent Store's in-memory index so subsequent List / Get calls
// see those writes.
//
// Without the watcher the index is populated exactly once at NewStore
// and only mutates through the parent process's own mutators — an
// MCP subprocess writing issues/<new>.json lands on disk but stays
// invisible to /api/v1/native/issues until the daemon restarts. The
// previous workaround (read the MCP tool's response JSON directly
// inside the runtime) hid the issue from the operator's perspective
// inside one run, but every other observer (studio /board view,
// dispatcher poller) still saw the stale state.
//
// Reload strategy is intentionally simple: any Create / Write /
// Rename / Remove event under issues/ re-reads the affected file
// (or drops it from the index on Remove) under the store's mutex.
// The daemon's own writes also fire fsnotify; the resulting re-read
// is wasteful but idempotent — much simpler than tracking "expected"
// vs "unexpected" events. With the current sub-1k-issue working set
// the redundant disk reads are noise; if a board ever grows past
// that, a "skip if matches just-written hash" optimisation can
// fit on top of this without changing the interface.
type indexWatcher struct {
	w    *fsnotify.Watcher
	stop chan struct{}
	done chan struct{}
}

// startIndexWatcher launches a goroutine that mirrors issues/ disk
// changes into s.index. Returns nil + no error when fsnotify is
// unavailable on the host (e.g. a read-only or kernel-restricted
// environment); the Store still works, it just can't see out-of-
// process writes — same as before this watcher existed.
func startIndexWatcher(s *Store) (*indexWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	issuesPath := filepath.Join(s.root, issuesDir)
	if err := w.Add(issuesPath); err != nil {
		_ = w.Close()
		// Missing directory shouldn't be fatal: NewStore already
		// ensured it exists, but a hostile filesystem (read-only
		// bind, container mounts) can still reject inotify_add_watch.
		// Surface the error so the caller decides whether to abort.
		return nil, err
	}
	iw := &indexWatcher{
		w:    w,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go iw.loop(s, issuesPath)
	return iw, nil
}

// Close stops the watcher goroutine and releases the fsnotify
// resources. Safe to call multiple times.
func (iw *indexWatcher) Close() error {
	if iw == nil {
		return nil
	}
	select {
	case <-iw.stop:
		return nil
	default:
		close(iw.stop)
	}
	<-iw.done
	return iw.w.Close()
}

func (iw *indexWatcher) loop(s *Store, issuesPath string) {
	defer close(iw.done)
	var mu sync.Mutex // guards seenErr only — store mutex covers index access.
	var seenErr bool
	for {
		select {
		case <-iw.stop:
			return
		case ev, ok := <-iw.w.Events:
			if !ok {
				return
			}
			if !relevantEvent(ev) {
				continue
			}
			id := idFromIssuePath(ev.Name, issuesPath)
			if id == "" {
				continue
			}
			applyEvent(s, id, ev.Op)
		case err, ok := <-iw.w.Errors:
			if !ok {
				return
			}
			// fsnotify error channel drains rare kernel queue-full
			// signals. Don't spam: log once per session. The Store
			// has no logger handle, so the best we can do is keep
			// the watcher alive — a missed event simply means the
			// daemon's view stays stale until the next event for
			// that file forces a refresh, or the next restart
			// repopulates from disk. Wrap the flag in a tiny mutex
			// because the events + errors selects run on the same
			// goroutine but the linter cannot prove that.
			mu.Lock()
			_ = seenErr
			_ = err
			seenErr = true
			mu.Unlock()
		}
	}
}

// relevantEvent screens out filesystem events that don't touch a
// committed issue file. The atomic-rename path writes to a `.tmp`
// shadow before moving it into place; we only react to the final
// `.json` filename so we don't reload from a half-written file.
// Chmod-only events are noise on Linux (umask churn during writes).
func relevantEvent(ev fsnotify.Event) bool {
	if ev.Op&fsnotify.Chmod == ev.Op {
		return false
	}
	name := filepath.Base(ev.Name)
	if !strings.HasSuffix(name, ".json") {
		return false
	}
	if strings.HasSuffix(name, ".tmp") || strings.Contains(name, ".tmp.") {
		return false
	}
	return true
}

// idFromIssuePath inverts issuePath: strip the issues/ prefix and
// `.json` suffix, then decode the encoded segment back to the issue
// id. Returns "" when the event isn't for a file we manage (e.g. a
// sub-directory, an unrelated entry that snuck into issues/).
func idFromIssuePath(eventPath, issuesPath string) string {
	rel, err := filepath.Rel(issuesPath, eventPath)
	if err != nil || rel == "." || strings.Contains(rel, string(filepath.Separator)) {
		return ""
	}
	encoded := strings.TrimSuffix(rel, ".json")
	if encoded == rel {
		return ""
	}
	return decodeID(encoded)
}

// applyEvent reconciles s.index with disk for one issue id. The
// store mutex serialises this against the parent process's own
// mutators so a watcher-driven reload can never race with a mutator
// that's mid-write — the disk read MUST happen inside the lock,
// otherwise a local mutator that runs between the read and the
// index update would have its newer state overwritten by the older
// snapshot we read pre-lock.
func applyEvent(s *Store, id string, op fsnotify.Op) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op&fsnotify.Remove == fsnotify.Remove {
		delete(s.index, id)
		return
	}
	iss, err := s.readIssueFromDisk(id)
	if err != nil {
		// File was removed between the event and our read, or the
		// JSON is malformed (concurrent half-write we didn't filter
		// out, or someone hand-edited it). Drop the cached entry
		// on ErrNotFound; leave it alone on other errors so the
		// stale-but-readable cached value beats a forced 404.
		if errors.Is(err, fs.ErrNotExist) {
			delete(s.index, id)
		}
		return
	}
	s.index[id] = iss
}

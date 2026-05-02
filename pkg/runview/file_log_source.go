package runview

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// startLogSource spawns a goroutine that tails the run's run.log file
// and pushes any appended bytes into the run's RunLogBuffer (which
// the editor server registered via prepareRunLogNoFile in detached
// mode). The buffer fans out to live WS subscribers.
//
// Mirrors startEventSource but operates on opaque byte streams rather
// than line-delimited JSON: log content is whatever the runner's
// iterion logger writes, with no assumed framing beyond newlines.
func startLogSource(s *Service, runID string, done <-chan struct{}) {
	go func() {
		path := filepath.Join(s.storeDir, "runs", runID, "run.log")
		tailLog(s, runID, path, done)
	}()
}

// tailLog is the long-running tail loop for run.log.
func tailLog(s *Service, runID, path string, done <-chan struct{}) {
	if !waitForFile(path, done, 5*time.Second) {
		// Fall through — the watch loop tolerates a missing file.
	}

	watcher, watcherErr := fsnotify.NewWatcher()
	if watcherErr != nil {
		s.logger.Warn("runview: log tail (%s): fsnotify unavailable, falling back to polling: %v", runID, watcherErr)
		tailLogPolling(s, runID, path, done)
		return
	}
	defer watcher.Close()

	dir := filepath.Dir(path)
	if err := watcher.Add(dir); err != nil {
		s.logger.Warn("runview: log tail (%s): watcher.Add(%q): %v — falling back to polling", runID, dir, err)
		tailLogPolling(s, runID, path, done)
		return
	}

	var offset int64
	offset = drainNewLogBytes(s, runID, path, offset)

	// Wide defensive poll — fsnotify is the fast path; this is the
	// safety net for dropped events on busy file systems.
	pollTicker := time.NewTicker(10 * time.Second)
	defer pollTicker.Stop()

	for {
		select {
		case <-done:
			drainNewLogBytes(s, runID, path, offset)
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Clean(ev.Name) != filepath.Clean(path) {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				offset = drainNewLogBytes(s, runID, path, offset)
			}
		case <-pollTicker.C:
			offset = drainNewLogBytes(s, runID, path, offset)
		case watcherErr := <-watcher.Errors:
			s.logger.Warn("runview: log tail (%s): watcher error: %v", runID, watcherErr)
		}
	}
}

// tailLogPolling is the fsnotify-less fallback.
func tailLogPolling(s *Service, runID, path string, done <-chan struct{}) {
	var offset int64
	offset = drainNewLogBytes(s, runID, path, offset)

	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-done:
			drainNewLogBytes(s, runID, path, offset)
			return
		case <-t.C:
			offset = drainNewLogBytes(s, runID, path, offset)
		}
	}
}

// drainNewLogBytes reads any bytes appended past `offset` and pushes
// them through the run's log buffer (which fans out to subscribers
// and persists nothing — the runner already wrote the canonical
// bytes to disk). Chunks are size-bounded to avoid pathological reads
// when the runner emits a megabyte burst.
const logChunkBudget = 64 * 1024

func drainNewLogBytes(s *Service, runID, path string, offset int64) int64 {
	buf := s.GetLogBuffer(runID)
	if buf == nil {
		// Subscription tore down before tailer caught up; nothing to do.
		return offset
	}

	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.logger.Warn("runview: log tail (%s): open: %v", runID, err)
		}
		return offset
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		s.logger.Warn("runview: log tail (%s): seek %d: %v — resetting to start", runID, offset, err)
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return offset
		}
		offset = 0
	}

	chunk := make([]byte, logChunkBudget)
	for {
		n, readErr := f.Read(chunk)
		if n > 0 {
			_, _ = buf.Write(chunk[:n])
			offset += int64(n)
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				s.logger.Warn("runview: log tail (%s): read: %v", runID, readErr)
			}
			return offset
		}
		if n < len(chunk) {
			return offset
		}
	}
}

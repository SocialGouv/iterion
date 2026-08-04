package runview

import (
	"io"
	"os"
	"path/filepath"
	"sync"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// ensureLogSource guarantees a run.log tailer is feeding a live RunLogBuffer
// for runID — the log-stream twin of ensureEventSource. For a run this process
// did NOT launch (an external `iterion run`/`resume`, a dispatcher-spawned
// run, or one re-attached across a restart) GetLogBuffer is nil, so the WS log
// subscribe used to fall back to a one-shot replay of run.log and the studio
// needed a full page refresh to see new lines. This starts a fresh in-memory
// buffer plus an fsnotify tailer (disk run.log → buffer) and returns the buffer
// to subscribe to, along with a refcounted release the caller MUST invoke once
// when its subscription ends (the tailer + buffer are dropped on the last
// release). A nil buffer means no source could be started.
//
// Guard the same way as ensureEventSource: only call for runs that are NOT
// active in-process (those feed their buffer directly via the logger tee) and
// are NOT terminal (a finished run's static run.log wants the one-shot replay,
// not a lingering tailer).
func (s *Service) ensureLogSource(runID string) (release func(), buf *RunLogBuffer) {
	s.fileSrcMu.Lock()
	if s.logSrcs == nil {
		s.logSrcs = make(map[string]*fileSrcHandle)
	}
	h := s.logSrcs[runID]
	if h == nil {
		h = &fileSrcHandle{done: make(chan struct{})}
		s.logSrcs[runID] = h
		s.prepareRunLogNoFile(runID)     // in-memory buffer GetLogBuffer will return
		startLogSource(s, runID, h.done) // fsnotify tail run.log → buffer
	}
	h.refs++
	s.fileSrcMu.Unlock()

	buf = s.GetLogBuffer(runID)

	var once sync.Once
	return func() {
		once.Do(func() {
			s.fileSrcMu.Lock()
			cur := s.logSrcs[runID]
			last := false
			if cur != nil {
				cur.refs--
				if cur.refs <= 0 {
					close(cur.done)
					delete(s.logSrcs, runID)
					last = true
				}
			}
			s.fileSrcMu.Unlock()
			if last {
				s.dropRunLog(runID)
			}
		})
	}, buf
}

// GetLogBuffer returns the live log buffer for runID, or nil if the
// run is not held by this process. Valid only while the run is
// active; the buffer is Close'd and removed when the run goroutine
// exits.
func (s *Service) GetLogBuffer(runID string) *RunLogBuffer {
	s.runLogsMu.RLock()
	defer s.runLogsMu.RUnlock()
	return s.runLogs[runID]
}

// logPositionForRun is the callback shape the store uses to stamp
// Event.LogOffset: returns the current byte total of the per-run log
// buffer, or 0 when no buffer exists yet (bootstrap events emitted
// before prepareRunLog ran). Cheap: one atomic read under an RLock.
func (s *Service) logPositionForRun(runID string) int64 {
	s.runLogsMu.RLock()
	buf := s.runLogs[runID]
	s.runLogsMu.RUnlock()
	if buf == nil {
		return 0
	}
	return buf.Total()
}

// registerRunEngine publishes the live Engine for runID so
// activeDurationForRun can read its monotonic active elapsed. Shares
// the runLogs lifecycle + mutex.
func (s *Service) registerRunEngine(runID string, eng *runtime.Engine, steer chan *runtime.OverrideMsg) {
	s.runLogsMu.Lock()
	s.runEngines[runID] = eng
	if steer != nil {
		s.runSteer[runID] = steer
	}
	s.runLogsMu.Unlock()
}

// unregisterRunEngine drops the Engine (and its steering channel) for
// runID once its goroutine exits.
func (s *Service) unregisterRunEngine(runID string) {
	s.runLogsMu.Lock()
	delete(s.runEngines, runID)
	// The workspace tracker's stat cache is per-run and the tracker
	// outlives every run in this process; evict it here with its peers.
	if s.workspaceTracker != nil {
		s.workspaceTracker.Forget(runID)
	}
	delete(s.runSteer, runID)
	s.runLogsMu.Unlock()
}

// forgetTrackerCacheIfNotLive evicts a run's workspace stat cache unless
// this process is executing that run.
//
// The tracker outlives every run in a studio process, and its per-run
// cache is only dropped by unregisterRunEngine — i.e. for runs this
// process EXECUTED. But the read-only surfaces (a node's file changes, a
// file diff) resolve labels for ARBITRARY runs, including long-finished
// ones this studio never ran, so each one browsed pinned its full stat
// cache for the life of the process: several MiB per run on a repo with
// vendored dependencies, growing monotonically with no ceiling as the
// operator pages through history.
//
// A live run keeps its warm cache — that one is on the write path, where
// re-reading index.json at every node boundary is exactly what the cache
// exists to avoid.
func (s *Service) forgetTrackerCacheIfNotLive(runID string) {
	if s == nil || s.workspaceTracker == nil || runID == "" {
		return
	}
	s.runLogsMu.RLock()
	_, live := s.runEngines[runID]
	s.runLogsMu.RUnlock()
	if !live {
		s.workspaceTracker.Forget(runID)
	}
}

// steerChannelFor returns the live run's override send channel, or nil
// when this process does not hold the run.
func (s *Service) steerChannelFor(runID string) chan *runtime.OverrideMsg {
	s.runLogsMu.RLock()
	defer s.runLogsMu.RUnlock()
	return s.runSteer[runID]
}

// activeDurationForRun is the store.ActiveDurationFn: returns the run's
// monotonic active duration in ms (engine SharedBudget elapsed), or 0
// when the run isn't held by this process or declares no budget. The
// value is CLOCK_MONOTONIC-derived (suspend-excluded) — never
// recomputed from wall-clock timestamps.
func (s *Service) activeDurationForRun(runID string) int64 {
	s.runLogsMu.RLock()
	eng := s.runEngines[runID]
	s.runLogsMu.RUnlock()
	if eng == nil {
		return 0
	}
	return eng.ActiveElapsed().Milliseconds()
}

// prepareRunLog creates a per-run log buffer (also persisting to
// <store-dir>/runs/<runID>/run.log when the store dir is writable)
// and wraps the service's writer + buffer into a per-run logger.
// Returns the buffer for cleanup and the logger to thread through
// both BuildExecutor and runtime.WithLogger so every iterion log line
// emitted during this run is captured for the WS subscribers.
func (s *Service) prepareRunLog(runID string) (*RunLogBuffer, *iterlog.Logger) {
	var filePath string
	if s.storeDir != "" {
		runDir := filepath.Join(s.storeDir, "runs", runID)
		if err := os.MkdirAll(runDir, 0o755); err == nil {
			filePath = filepath.Join(runDir, "run.log")
		}
	}
	buf, fileErr := NewRunLogBuffer(filePath)
	if fileErr != nil {
		s.logger.Warn("runview: open run.log for %s: %v — proceeding without disk persistence", runID, fileErr)
	}

	s.runLogsMu.Lock()
	if old, ok := s.runLogs[runID]; ok {
		// Defensive: a previous run goroutine for this ID didn't
		// fully clean up. The store lock should make this impossible,
		// but if it ever happens we want the WS subscribers of the
		// stale buffer to see EOF rather than dangle forever.
		old.Close()
	}
	s.runLogs[runID] = buf
	s.runLogsMu.Unlock()

	perRunLogger := iterlog.New(s.logger.Level(), io.MultiWriter(s.logger.Writer(), buf))
	return buf, perRunLogger
}

// prepareRunLogNoFile is the detached-mode counterpart to
// prepareRunLog: it installs an in-memory-only buffer for runID
// (no file tee) and does NOT return a logger. The runner subprocess
// owns the on-disk run.log; a second writer here would corrupt it.
// File contents reach this buffer via the file_log_source tailer,
// which reads new bytes off disk and pushes them through Write.
func (s *Service) prepareRunLogNoFile(runID string) *RunLogBuffer {
	buf, _ := NewRunLogBuffer("")
	s.runLogsMu.Lock()
	if old, ok := s.runLogs[runID]; ok {
		old.Close()
	}
	s.runLogs[runID] = buf
	s.runLogsMu.Unlock()
	return buf
}

// dropRunLog tears down the per-run buffer at run-completion time:
// closes any active subscribers, the persisted file, and removes the
// map entry. Idempotent.
func (s *Service) dropRunLog(runID string) {
	s.runLogsMu.Lock()
	buf := s.runLogs[runID]
	delete(s.runLogs, runID)
	s.runLogsMu.Unlock()
	if buf != nil {
		buf.Close()
	}
}

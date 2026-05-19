package runview

import (
	"io"
	"os"
	"path/filepath"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

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

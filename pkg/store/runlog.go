package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TeeRunLog opens <storeRoot>/runs/<runID>/run.log and returns a new
// logger whose output is multiplexed to both stderr and the file. The
// runID is validated before any path is constructed so a hostile CLI or
// dispatcher-supplied ID cannot create/open run.log outside the store and
// only fail later when CreateRun validates the ID.
//
// On error the original logger is returned with a nil closer so callers
// can keep running — a run with no writable store dir still works (logs
// go to stderr only). The returned closer is nil when no tee was set up;
// callers should defer Close on the non-nil result.
//
// Shared between the CLI runner (pkg/cli/run.go) and the in-process
// dispatcher runner (pkg/dispatcher/engine_runner.go) so dispatched runs
// produce the same per-run log file the studio's log viewer expects —
// without this, the studio renders "No log captured" on every
// dispatcher-spawned run because the file simply doesn't exist.
func TeeRunLog(logger *iterlog.Logger, level iterlog.Level, storeRoot, runID string) (*iterlog.Logger, io.Closer) {
	warn := func(format string, args ...any) {
		if logger != nil {
			logger.Warn(format, args...)
		}
	}
	if err := SanitizePathComponent("run ID", runID); err != nil {
		warn("store: refusing run.log tee for unsafe run ID: %v", err)
		return logger, nil
	}

	runsDir := filepath.Join(storeRoot, "runs")
	runDir := filepath.Join(runsDir, runID)
	if err := os.MkdirAll(runDir, dirPerm); err != nil {
		warn("store: mkdir run dir for log tee: %v", err)
		return logger, nil
	}
	// MkdirAll does not tighten existing directories, and this helper can be
	// the first store code path to touch <root>/runs/<runID>. Run logs can
	// contain prompts, model outputs, and secrets, so force the private store
	// modes even when an older build left the directory world-readable.
	if err := os.Chmod(runsDir, dirPerm); err != nil {
		warn("store: chmod runs dir for log tee: %v", err)
		return logger, nil
	}
	if err := os.Chmod(runDir, dirPerm); err != nil {
		warn("store: chmod run dir for log tee: %v", err)
		return logger, nil
	}

	logPath := filepath.Join(runDir, "run.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		warn("store: open run.log for tee: %v", err)
		return logger, nil
	}
	if err := logFile.Chmod(filePerm); err != nil {
		_ = logFile.Close()
		warn("store: chmod run.log for tee: %v", err)
		return logger, nil
	}
	return iterlog.New(level, io.MultiWriter(os.Stderr, logFile)), logFile
}

// runLogPath validates runID and returns <root>/runs/<runID>/run.log.
func (s *FilesystemRunStore) runLogPath(runID string) (string, error) {
	if err := SanitizePathComponent("run ID", runID); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "runs", runID, "run.log"), nil
}

// AppendRunLog implements RunLogStore over runs/<id>/run.log. Bytes are
// written at their absolute offset (WriteAt, not O_APPEND) so an
// idempotent redelivery of an already-persisted chunk rewrites the same
// bytes instead of corrupting the stream. The normal local writer is
// the RunLogBuffer file tee — this method exists for store-API parity
// (a runner pointed at a filesystem store).
func (s *FilesystemRunStore) AppendRunLog(_ context.Context, runID string, offset int64, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if offset < 0 {
		return fmt.Errorf("store: AppendRunLog(%s): negative offset %d", runID, offset)
	}
	logPath, err := s.runLogPath(runID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), dirPerm); err != nil {
		return fmt.Errorf("store: AppendRunLog(%s): mkdir: %w", runID, err)
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return fmt.Errorf("store: AppendRunLog(%s): open: %w", runID, err)
	}
	defer f.Close()
	if _, err := f.WriteAt(data, offset); err != nil {
		return fmt.Errorf("store: AppendRunLog(%s): write at %d: %w", runID, offset, err)
	}
	return nil
}

// ReadRunLogRange implements RunLogStore: bytes [from, until) of
// runs/<id>/run.log, until <= 0 meaning "to end". A missing file is
// (nil, nil) — the run produced no log.
func (s *FilesystemRunStore) ReadRunLogRange(_ context.Context, runID string, from, until int64) ([]byte, error) {
	logPath, err := s.runLogPath(runID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: ReadRunLogRange(%s): open: %w", runID, err)
	}
	defer f.Close()
	if from < 0 {
		from = 0
	}
	if until <= 0 {
		st, err := f.Stat()
		if err != nil {
			return nil, fmt.Errorf("store: ReadRunLogRange(%s): stat: %w", runID, err)
		}
		until = st.Size()
	}
	if from >= until {
		return nil, nil
	}
	buf := make([]byte, until-from)
	n, err := f.ReadAt(buf, from)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("store: ReadRunLogRange(%s): read at %d: %w", runID, from, err)
	}
	return buf[:n], nil
}

// RunLogSize implements RunLogStore: the persisted byte count of
// runs/<id>/run.log (0 when missing).
func (s *FilesystemRunStore) RunLogSize(_ context.Context, runID string) (int64, error) {
	logPath, err := s.runLogPath(runID)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: RunLogSize(%s): stat: %w", runID, err)
	}
	return st.Size(), nil
}

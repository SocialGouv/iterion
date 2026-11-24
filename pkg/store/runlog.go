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
// On error a FORK of the original logger is returned with a nil closer so
// callers can keep running — a run with no writable store dir still works
// (logs go to stderr only). The fork is what makes the returned handle the
// RUN's: BuildExecutor sets the run's secret matcher on the logger a
// surface hands it, and that slot is per-handle, so returning the caller's
// own logger here would install one run's matcher where the next run
// replaces it (iterion#2822). The returned closer is nil when no tee was
// set up; callers should defer Close on the non-nil result.
//
// This wrapper preserves the context-free historical compatibility API.
// CLI and dispatcher invocations use TeeRunLogContext with their original
// binding, then apply RunLogErrorFatal to the returned error.
func TeeRunLog(logger *iterlog.Logger, level iterlog.Level, storeRoot, runID string) (*iterlog.Logger, io.Closer) {
	out, closer, err := TeeRunLogContext(context.Background(), logger, level, storeRoot, runID)
	if err != nil && logger != nil {
		logger.Warn("store: run.log tee refused: %v", err)
	}
	return out, closer
}

// TeeRunLogContext validates the original Run format before opening a file.
// Delayed callers must pass their captured binding and propagate the error.
// Its returned descriptor refers to this inode; it is not reopened by path.
// TeeRunLog retains the historical stderr-only fallback for legacy callers.
func TeeRunLogContext(ctx context.Context, logger *iterlog.Logger, level iterlog.Level, storeRoot, runID string) (*iterlog.Logger, io.Closer, error) {
	logger = logger.WithOwnRedactor()
	if err := SanitizePathComponent("run ID", runID); err != nil {
		return logger, nil, err
	}
	if ctx == nil {
		return logger, nil, ErrInvalidValueFormat
	}
	if err := ctx.Err(); err != nil {
		return logger, nil, err
	}
	s, err := New(storeRoot)
	if err != nil {
		return logger, nil, err
	}
	_, unlock, err := s.lockRunMetadataMutation(ctx, runID)
	if err != nil {
		return logger, nil, err
	}
	defer unlock()
	payload, err := s.openRunLog(ctx, runID, true)
	if err != nil {
		return logger, nil, err
	}
	defer payload.Close()
	if !payload.strict {
		runDir := filepath.Dir(payload.key)
		runsDir := filepath.Dir(runDir)
		// Preserve historical permission hardening for the legacy namespace.
		if err = os.Chmod(runsDir, dirPerm); err != nil {
			return logger, nil, err
		}
		if err = os.Chmod(runDir, dirPerm); err != nil {
			return logger, nil, err
		}
	}
	logFile, err := payload.OpenFile(os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return logger, nil, err
	}
	if err = logFile.Chmod(filePerm); err != nil {
		_ = logFile.Close()
		return logger, nil, err
	}
	if payload.strict {
		if err = errors.Join(logFile.Sync(), payload.syncParent()); err != nil {
			_ = logFile.Close()
			return logger, nil, err
		}
	}
	if err = ctx.Err(); err != nil {
		_ = logFile.Close()
		return logger, nil, err
	}
	return iterlog.New(level, io.MultiWriter(os.Stderr, logFile)), logFile, nil
}

// openRunLog validates the binding and retains the selected log namespace.
func (s *FilesystemRunStore) openRunLog(ctx context.Context, runID string, create bool) (*runPayloadFile, error) {
	return s.openRunPayloadFile(ctx, runID, "run.log", create)
}

// AppendRunLog implements RunLogStore over runs/<id>/run.log. Bytes are
// written at their absolute offset (WriteAt, not O_APPEND) so an
// idempotent redelivery of an already-persisted chunk rewrites the same
// bytes instead of corrupting the stream. The normal local writer is
// the RunLogBuffer file tee — this method exists for store-API parity
// (a runner pointed at a filesystem store).
func (s *FilesystemRunStore) AppendRunLog(ctx context.Context, runID string, offset int64, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if offset < 0 {
		return fmt.Errorf("store: AppendRunLog(%s): negative offset %d", runID, offset)
	}
	_, unlock, err := s.lockRunMetadataMutation(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	payload, err := s.openRunLog(ctx, runID, true)
	if err != nil {
		return err
	}
	defer payload.Close()
	f, err := payload.OpenFile(os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return fmt.Errorf("store: AppendRunLog(%s): open: %w", runID, err)
	}
	defer f.Close()
	if _, err := f.WriteAt(data, offset); err != nil {
		return fmt.Errorf("store: AppendRunLog(%s): write at %d: %w", runID, offset, err)
	}
	if payload.strict {
		return errors.Join(f.Sync(), payload.syncParent())
	}
	return nil
}

// ReadRunLogRange implements RunLogStore: bytes [from, until) of
// runs/<id>/run.log, until <= 0 meaning "to end". A missing file is
// (nil, nil) — the run produced no log.
func (s *FilesystemRunStore) ReadRunLogRange(ctx context.Context, runID string, from, until int64) ([]byte, error) {
	payload, err := s.openRunLog(ctx, runID, false)
	if err != nil {
		return nil, err
	}
	defer payload.Close()
	f, err := payload.Open()
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
func (s *FilesystemRunStore) RunLogSize(ctx context.Context, runID string) (int64, error) {
	payload, err := s.openRunLog(ctx, runID, false)
	if err != nil {
		return 0, err
	}
	defer payload.Close()
	st, err := payload.Stat()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: RunLogSize(%s): stat: %w", runID, err)
	}
	return st.Size(), nil
}

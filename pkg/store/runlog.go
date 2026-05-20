package store

import (
	"io"
	"os"
	"path/filepath"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TeeRunLog opens <runDir>/run.log and returns a new logger whose
// output is multiplexed to both stderr and the file. On error the
// original logger is returned with a nil closer so callers can keep
// running — a run with no writable store dir still works (logs go
// to stderr only). The returned closer is nil when no tee was set
// up; callers should defer Close on the non-nil result.
//
// Shared between the CLI runner (pkg/cli/run.go) and the in-process
// dispatcher runner (pkg/dispatcher/engine_runner.go) so dispatched
// runs produce the same per-run log file the studio's log viewer
// expects — without this, the studio renders "No log captured" on
// every dispatcher-spawned run because the file simply doesn't exist.
func TeeRunLog(logger *iterlog.Logger, level iterlog.Level, runDir string) (*iterlog.Logger, io.Closer) {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		logger.Warn("store: mkdir run dir for log tee: %v", err)
		return logger, nil
	}
	logFile, err := os.OpenFile(filepath.Join(runDir, "run.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		logger.Warn("store: open run.log for tee: %v", err)
		return logger, nil
	}
	return iterlog.New(level, io.MultiWriter(os.Stderr, logFile)), logFile
}

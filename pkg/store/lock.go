package store

import (
	"context"
	"fmt"
)

// RunLock represents an exclusive advisory lock on a run directory.
// The lock prevents concurrent processes from modifying the same run.
type RunLock interface {
	// Unlock releases the lock. Must be called when done.
	Unlock() error
}

// LockRun acquires an exclusive file-based lock for the given run.
// On Unix, it uses flock(2) which is automatically released on process crash.
// On Windows, it uses a lockfile with PID-based stale detection.
//
// The lock is advisory: it protects against concurrent iterion processes
// sharing the same store directory. It does not replace the internal
// sync.Mutex which handles intra-process concurrency.
//
// Limitations: does not work over NFS (flock is local-only on Linux).
func (s *FilesystemRunStore) LockRun(_ context.Context, runID string) (RunLock, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	dir := s.runDir(runID)
	return lockRun(dir, fmt.Sprintf("run %s", runID))
}

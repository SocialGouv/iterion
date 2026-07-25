package store

import (
	"context"
	"fmt"
	"time"
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

// AcquireFileLock returns an exclusive lock pinned to a specific file
// path. It is the directory-agnostic surface of the same flock /
// PID-lock primitive that backs LockRun, exposed so consumers outside
// pkg/store (typically the dispatcher's process-level lock on the
// workspace root) can reuse the implementation instead of reinventing
// it cross-platform.
//
// The lock is non-blocking — if another process already holds the
// file, AcquireFileLock returns an error rather than waiting. The OS
// releases the lock automatically when the holding process exits.
func AcquireFileLock(path, label string) (RunLock, error) {
	return lockFile(path, label)
}

// acquireFileLockRetry is the short-critical-section variant of
// AcquireFileLock: instead of failing on contention it retries until
// timeout. Used where the protected section is a few milliseconds
// (an interaction check-and-set) and the correct behaviour under
// contention is to wait for the other writer, not to error.
func acquireFileLockRetry(path, label string, timeout time.Duration) (RunLock, error) {
	deadline := time.Now().Add(timeout)
	for {
		l, err := lockFile(path, label)
		if err == nil {
			return l, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("store: %s: lock contention persisted for %s: %w", label, timeout, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

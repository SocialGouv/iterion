//go:build windows

package store

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// fileLock is an exclusive LockFileEx lock on an open handle — the Windows
// counterpart of lock_unix.go's flock: Windows releases it when the handle
// closes or the process dies, and two handles conflict even within one
// process, as two flock descriptions do.
//
// It replaces a pid-in-a-file lock (O_CREATE|O_EXCL, reclaimed when the pid
// it named was dead), which broke under contention inside one process: a
// contender reading the file between its creation and its pid write took it
// for corrupt and deleted it, so both held the lock; and Unlock's remove
// fails while a contender has the file open for reading, leaving a lock
// that names a live pid (ours) and that nothing reclaims until restart.
type fileLock struct {
	fl *flock.Flock
}

func (l *fileLock) Unlock() error {
	return l.fl.Unlock()
}

// lockRun acquires an exclusive lock on <runDir>/.lock.
// The OS releases it if the process crashes.
func lockRun(runDir, label string) (RunLock, error) {
	if err := os.MkdirAll(runDir, dirPerm); err != nil {
		return nil, fmt.Errorf("store: mkdir for lock: %w", err)
	}
	return lockFile(filepath.Join(runDir, ".lock"), label)
}

// lockFile is the shared primitive used by lockRun and the exported
// AcquireFileLock. It locks an exact path (caller is responsible for
// ensuring the parent directory exists).
func lockFile(path, label string) (RunLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("store: mkdir for lock: %w", err)
	}
	fl := flock.New(path, flock.SetPermissions(filePerm))
	ok, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("store: open lock file for %s: %w", label, err)
	}
	if !ok {
		return nil, fmt.Errorf("store: %s is locked by another process", label)
	}
	return &fileLock{fl: fl}, nil
}

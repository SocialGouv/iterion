//go:build !windows

package store

import (
	"errors"
	"os"
	"syscall"
)

// Existing reader-accessible lock files use the very same advisory inode.
// Opening read-only avoids requiring payload observers to write the lock.
func tryStreamReadLock(path, _ string) (RunLock, streamReadLockOutcome, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if errors.Is(e, os.ErrNotExist) {
		return nil, streamReadLockQuiescent, nil
	}
	if e != nil {
		// A link where the lock belongs, or a lock path through a file, is a
		// replaced node; a permission or descriptor failure stays transient.
		return nil, streamReadLockFailure, streamNodeError(e)
	}
	info, e := f.Stat()
	if e != nil {
		f.Close()
		return nil, streamReadLockFailure, streamNodeError(e)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, streamReadLockFailure, ErrRunDataIdentityChanged
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		if errors.Is(e, syscall.EWOULDBLOCK) || errors.Is(e, syscall.EAGAIN) {
			return nil, streamReadLockBusy, e
		}
		return nil, streamReadLockFailure, e
	}
	return &flockLock{f: f}, streamReadLockOwned, nil
}

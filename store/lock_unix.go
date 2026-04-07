//go:build !windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type flockLock struct {
	f *os.File
}

func (l *flockLock) Unlock() error {
	if err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); err != nil {
		l.f.Close()
		return fmt.Errorf("store: flock unlock: %w", err)
	}
	return l.f.Close()
}

// lockRun acquires an exclusive flock on <runDir>/.lock.
// The OS automatically releases the lock if the process crashes.
func lockRun(runDir, label string) (RunLock, error) {
	if err := os.MkdirAll(runDir, dirPerm); err != nil {
		return nil, fmt.Errorf("store: mkdir for lock: %w", err)
	}
	p := filepath.Join(runDir, ".lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, fmt.Errorf("store: open lock file for %s: %w", label, err)
	}
	// LOCK_EX: exclusive, blocking (waits if another process holds it).
	// Use LOCK_NB for non-blocking: fail immediately if locked.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("store: %s is locked by another process", label)
	}
	return &flockLock{f: f}, nil
}

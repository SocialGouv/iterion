//go:build windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type pidLock struct {
	path string
}

func (l *pidLock) Unlock() error {
	return os.Remove(l.path)
}

// lockRun acquires an exclusive lockfile at <runDir>/.lock using O_CREATE|O_EXCL.
// If a stale lockfile exists (PID no longer running), it is removed and retried.
func lockRun(runDir, label string) (RunLock, error) {
	if err := os.MkdirAll(runDir, dirPerm); err != nil {
		return nil, fmt.Errorf("store: mkdir for lock: %w", err)
	}
	p := filepath.Join(runDir, ".lock")
	pid := os.Getpid()

	// Try to create exclusively.
	if err := tryCreateLockfile(p, pid); err == nil {
		return &pidLock{path: p}, nil
	}

	// File exists — check if the owning process is still alive.
	if removeStaleLock(p) {
		if err := tryCreateLockfile(p, pid); err == nil {
			return &pidLock{path: p}, nil
		}
	}

	return nil, fmt.Errorf("store: %s is locked by another process", label)
}

func tryCreateLockfile(path string, pid int) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%d", pid)
	return err
}

// removeStaleLock reads the PID from the lockfile and removes it if the
// content is corrupt (non-numeric). On Windows, reliably checking whether a
// PID is alive without CGO or x/sys/windows is not possible (FindProcess
// always succeeds), so we only remove clearly corrupt lockfiles.
// If a process crashes, the user must delete .lock manually.
func removeStaleLock(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if _, err := strconv.Atoi(strings.TrimSpace(string(data))); err != nil {
		// Corrupt lockfile (non-numeric content) — safe to remove.
		os.Remove(path)
		return true
	}
	return false
}

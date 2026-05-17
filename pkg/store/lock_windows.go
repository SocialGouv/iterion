//go:build windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
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
	return lockFile(filepath.Join(runDir, ".lock"), label)
}

// lockFile is the shared primitive used by lockRun and the exported
// AcquireFileLock. It locks an exact path (caller is responsible for
// ensuring the parent directory exists).
func lockFile(path, label string) (RunLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("store: mkdir for lock: %w", err)
	}
	pid := os.Getpid()
	if err := tryCreateLockfile(path, pid); err == nil {
		return &pidLock{path: path}, nil
	}
	if removeStaleLock(path) {
		if err := tryCreateLockfile(path, pid); err == nil {
			return &pidLock{path: path}, nil
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

// removeStaleLock removes the lockfile when the PID it names is no
// longer running or the content is corrupt. The previous incarnation
// only removed corrupt lockfiles because Go's os.FindProcess always
// "succeeds" on Windows; we now ask Windows directly via
// OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION) — the call returns
// ERROR_INVALID_PARAMETER for an unknown PID, which is the signal we
// use to declare the lockfile stale and reclaim it.
func removeStaleLock(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		// Corrupt lockfile (non-numeric content) — safe to remove.
		os.Remove(path)
		return true
	}
	if pid <= 0 {
		os.Remove(path)
		return true
	}
	if !pidAliveWindows(uint32(pid)) {
		os.Remove(path)
		return true
	}
	return false
}

// pidAliveWindows reports whether a process with the given PID exists.
// Uses OpenProcess with PROCESS_QUERY_LIMITED_INFORMATION (the minimum
// access right that lets a non-admin process probe an arbitrary PID
// for liveness). A successful open means the PID is live; an
// ERROR_INVALID_PARAMETER means it never existed or has fully exited
// and been reaped from the kernel's process table.
func pidAliveWindows(pid uint32) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		// Conservative: anything other than the explicit "no such PID"
		// signal is treated as "still alive" so we never reclaim a
		// lockfile by accident on access-denied or transient errors.
		return err != windows.ERROR_INVALID_PARAMETER
	}
	windows.CloseHandle(handle)
	return true
}

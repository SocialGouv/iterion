package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// pidFileName records the detached runner PID under the run directory.
const pidFileName = ".pid"

// PIDFilePath returns the canonical path; the file need not exist.
func (s *FilesystemRunStore) PIDFilePath(runID string) string {
	return filepath.Join(s.root, "runs", runID, pidFileName)
}

// WritePIDFile preserves the legacy orphan-sidecar API. Invocations use the
// contextual capability, which requires their original format and parent.
func (s *FilesystemRunStore) WritePIDFile(runID string, pid int) error {
	_, unlock, err := s.lockRunMetadataMutation(context.Background(), runID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.writePIDFileLocked(runID, pid)
}

// WritePIDFileContext atomically replaces the PID of an existing bound run.
func (s *FilesystemRunStore) WritePIDFileContext(ctx context.Context, runID string, pid int) error {
	unlock, err := s.lockInvocationPID(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.writePIDFileLocked(runID, pid)
}

func (s *FilesystemRunStore) writePIDFileLocked(runID string, pid int) error {
	dir := filepath.Join(s.root, "runs", runID)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: pid file: mkdir: %w", err)
	}
	return writeFileAtomic(filepath.Join(dir, pidFileName), []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// ReadPIDFile is the legacy orphan-sidecar reader. A missing PID returns zero;
// malformed contents return an error. It does not bind a run invocation.
func (s *FilesystemRunStore) ReadPIDFile(runID string) (int, error) {
	return s.readPIDFileNoLock(runID)
}

// ReadPIDFileContext reads under the original run's metadata lock. A missing
// parent is an error; a missing PID in an existing parent returns zero.
func (s *FilesystemRunStore) ReadPIDFileContext(ctx context.Context, runID string) (int, error) {
	unlock, err := s.lockInvocationPID(ctx, runID)
	if err != nil {
		return 0, err
	}
	defer unlock()
	return s.readPIDFileNoLock(runID)
}

func (s *FilesystemRunStore) readPIDFileNoLock(runID string) (int, error) {
	data, err := os.ReadFile(s.PIDFilePath(runID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: pid file: read: %w", err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return 0, fmt.Errorf("store: pid file: empty for run %q", runID)
	}
	pid, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("store: pid file: parse %q: %w", trimmed, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("store: pid file: invalid pid %d", pid)
	}
	return pid, nil
}

// RemovePIDFile preserves the legacy orphan-sidecar cleanup API.
func (s *FilesystemRunStore) RemovePIDFile(runID string) error {
	_, unlock, err := s.lockRunMetadataMutation(context.Background(), runID)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = s.removePIDFileLocked(runID)
	return err
}

// RemovePIDFileContext removes a PID under its original run's metadata lock.
// Delayed owners use RemovePIDFileIfContext to retain a replacement PID.
func (s *FilesystemRunStore) RemovePIDFileContext(ctx context.Context, runID string) error {
	unlock, err := s.lockInvocationPID(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = s.removePIDFileLocked(runID)
	return err
}

// RemovePIDFileIfContext compares and removes under one stable metadata lock.
// A different or absent PID is a successful no-op. Format and parent refusal
// remain errors even when no PID file exists.
func (s *FilesystemRunStore) RemovePIDFileIfContext(ctx context.Context, runID string, expected int) (bool, error) {
	if expected <= 0 {
		return false, fmt.Errorf("store: expected PID must be positive")
	}
	unlock, err := s.lockInvocationPID(ctx, runID)
	if err != nil {
		return false, err
	}
	defer unlock()
	current, err := s.readPIDFileNoLock(runID)
	if err != nil {
		return false, err
	}
	if current == 0 || current != expected {
		return false, nil
	}
	return s.removePIDFileLocked(runID)
}

func (s *FilesystemRunStore) removePIDFileLocked(runID string) (bool, error) {
	err := os.Remove(s.PIDFilePath(runID))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: pid file: remove: %w", err)
	}
	return true, nil
}

func (s *FilesystemRunStore) lockInvocationPID(ctx context.Context, runID string) (func(), error) {
	if err := RequireRunValueFormat(ctx, runID); err != nil {
		return nil, err
	}
	_, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	return unlock, err
}

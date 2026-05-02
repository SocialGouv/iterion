package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// pidFileName lives under <store>/runs/<runID>/.pid. It records the
// PID of the iterion-run subprocess managing this run on behalf of an
// editor server. Presence of the file marks the run as "managed by a
// detached runner"; absence means in-process or already-cleaned-up.
const pidFileName = ".pid"

// PIDFilePath returns the canonical path to the .pid file for runID.
// The file may or may not exist.
func (s *RunStore) PIDFilePath(runID string) string {
	return filepath.Join(s.root, "runs", runID, pidFileName)
}

// WritePIDFile writes pid to the run's .pid file. The directory is
// created if it doesn't exist. Writes are atomic via writeFileAtomic
// (tmp + fsync + rename) so a crashed writer cannot leave a
// half-written .pid that would confuse the reconciler.
func (s *RunStore) WritePIDFile(runID string, pid int) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	dir := filepath.Join(s.root, "runs", runID)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: pid file: mkdir: %w", err)
	}
	return writeFileAtomic(filepath.Join(dir, pidFileName), []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// ReadPIDFile returns the PID recorded for runID, or 0 + nil if no
// .pid file exists. A malformed file returns 0 + an error so callers
// can decide whether to clean it up.
func (s *RunStore) ReadPIDFile(runID string) (int, error) {
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

// RemovePIDFile deletes the run's .pid file. Idempotent — missing
// files are not errors. Called by the runner subprocess on exit and
// by the reconciler when it detects a dead PID.
func (s *RunStore) RemovePIDFile(runID string) error {
	err := os.Remove(s.PIDFilePath(runID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("store: pid file: remove: %w", err)
	}
	return nil
}

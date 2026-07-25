package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DeleteRun permanently removes a run's entire directory (run.json,
// events.jsonl, artifacts, interactions, attachments — everything under
// runs/<id>/). Idempotent: a missing run dir is not an error.
// deletionMarkerName is the durable tombstone DeleteRun leaves behind:
// a single file inside the (otherwise emptied) run directory. Every
// writer checks it before creating anything, so a late writer cannot
// resurrect a deleted run by re-creating its tree — the historical
// failure mode of a bare RemoveAll (AppendEvent's MkdirAll would
// happily rebuild the directory). Reaped by `iterion runs prune`.
// Deliberately NOT the dispatcher's in-memory `tombstones` map — that
// is a live slot-holder, this is a durable deletion marker.
const deletionMarkerName = ".deleted"

// runDeleted reports whether the run carries the deletion marker.
func (s *FilesystemRunStore) runDeleted(id string) bool {
	_, err := os.Stat(filepath.Join(s.root, "runs", id, deletionMarkerName))
	return err == nil
}

// guardNotDeleted is the shared write-path check: a typed refusal for
// tombstoned runs, before any directory or file would be (re)created.
func (s *FilesystemRunStore) guardNotDeleted(id string) error {
	if s.runDeleted(id) {
		return fmt.Errorf("store: run %s: %w", id, ErrRunDeleted)
	}
	return nil
}

func (s *FilesystemRunStore) DeleteRun(_ context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("store: DeleteRun requires a run id")
	}
	if err := sanitizePathComponent("run ID", id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.root, "runs", id)
	// Write the tombstone FIRST (MkdirAll is idempotent if the dir was
	// already gone), so a concurrent writer that passes its guard just
	// before our sweep still finds the marker on its next write.
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: delete run %s: %w", id, err)
	}
	marker := filepath.Join(dir, deletionMarkerName)
	if err := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), filePerm); err != nil {
		return fmt.Errorf("store: write deletion marker for %s: %w", id, err)
	}
	// Remove everything EXCEPT the marker.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("store: delete run %s: %w", id, err)
	}
	for _, e := range entries {
		if e.Name() == deletionMarkerName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("store: delete run %s content %s: %w", id, e.Name(), err)
		}
	}
	return nil
}

// PruneDeletionMarkers removes tombstone directories whose marker is
// older than cutoff. The marker's mtime is its creation instant.
func (s *FilesystemRunStore) PruneDeletionMarkers(_ context.Context, cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runsDir := filepath.Join(s.root, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: prune deletion markers: %w", err)
	}
	reaped := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		marker := filepath.Join(runsDir, e.Name(), deletionMarkerName)
		info, err := os.Stat(marker)
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(runsDir, e.Name())); err != nil {
			return reaped, fmt.Errorf("store: reap tombstone %s: %w", e.Name(), err)
		}
		reaped++
	}
	return reaped, nil
}

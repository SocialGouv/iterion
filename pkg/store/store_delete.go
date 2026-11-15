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

func (s *FilesystemRunStore) DeleteRun(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("store: DeleteRun requires a run id")
	}
	if err := sanitizePathComponent("run ID", id); err != nil {
		return err
	}
	if err := s.deleteRunContent(ctx, id); err != nil {
		return err
	}
	// The credential-slot ledger (credslot_fs.go) names the run inside the
	// holders of every key it was admitted on: pulled now, so a deleted run
	// frees its slot at once rather than when a refusal's reconcile finds
	// its document gone past CredSlotGrace. Outside mu: the ledger has its
	// own lock, and nothing takes mu then it.
	s.dropCredSlotHolder(id)
	return nil
}

// deleteRunContent writes the tombstone and removes the run's content under
// mu, which it takes itself (a *Locked helper is one whose caller holds mu).
func (s *FilesystemRunStore) deleteRunContent(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.acquireRunWriteLock(id)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	if err := s.checkDeletionHeaderLocked(ctx, id); err != nil {
		return err
	}
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
func (s *FilesystemRunStore) PruneDeletionMarkers(ctx context.Context, cutoff time.Time) (int, error) {
	if ctx == nil {
		return 0, ErrInvalidValueFormat
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
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
		// Select candidates without contending with unrelated live writers.
		// The observation grants no permission to delete: the marker and its
		// date are checked again after acquiring this candidate's stable lock.
		info, err := os.Stat(filepath.Join(runsDir, e.Name(), deletionMarkerName))
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		pruned, err := s.pruneDeletionMarker(ctx, e.Name(), cutoff)
		if err != nil {
			return reaped, err
		}
		if pruned {
			reaped++
		}
	}
	return reaped, nil
}

// The scan is only a candidate list. Re-read the marker under the same lock
// as creation/deletion, and release it before visiting another run ID.
func (s *FilesystemRunStore) pruneDeletionMarker(ctx context.Context, id string, cutoff time.Time) (bool, error) {
	lock, err := s.acquireRunWriteLock(id)
	if err != nil {
		return false, err
	}
	defer func() { _ = lock.Unlock() }()
	dir := s.runDir(id)
	info, err := os.Stat(filepath.Join(dir, deletionMarkerName))
	if err != nil || info.ModTime().After(cutoff) {
		return false, nil
	}
	if err := s.checkDeletionHeaderLocked(ctx, id); err != nil {
		return false, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("store: reap tombstone %s: %w", id, err)
	}
	return true, nil
}

// A failed cleanup can retain run.json beside its marker. Validate its raw
// header even though ordinary loadRunRaw refuses tombstones. Completed old
// tombstones have no document; this does not invent a new marker format.
func (s *FilesystemRunStore) checkDeletionHeaderLocked(ctx context.Context, id string) error {
	if ctx == nil {
		return ErrInvalidValueFormat
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := os.ReadFile(s.runJSONPath(id))
	format := LegacyValueFormat
	if err == nil {
		format, err = RunValueFormatFromJSON(ctx, raw)
	} else if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return err
	}
	r := &Run{ID: id, FormatVersion: int(format)}
	_, err = CheckRunFormatReplacement(ctx, r, r)
	return err
}

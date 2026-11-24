package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	if ctx == nil {
		return ErrInvalidValueFormat
	}
	scratchLock, err := s.lockCodemodeScratch(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = scratchLock.Unlock() }()
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
	// Preserve the tenant of a scoped run even after the sweep removes run.json.
	// A repeat deletion leaves the existing marker (and owner) intact.
	if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
		body := []byte(time.Now().UTC().Format(time.RFC3339Nano) + "\n")
		raw, err := os.ReadFile(s.runJSONPath(id))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("store: read deletion marker owner for %s: %w", id, err)
		}
		if err == nil {
			tenant, err := streamCanonicalTenant(ctx, raw)
			if err != nil {
				return fmt.Errorf("store: read deletion marker owner for %s: %w", id, err)
			}
			if tenant != "" {
				owner, err := json.Marshal(tenant)
				if err != nil {
					return fmt.Errorf("store: encode deletion marker tenant: %w", err)
				}
				body = append(body, owner...)
			}
		}
		if err := WriteFileAtomic(marker, body, filePerm); err != nil {
			return fmt.Errorf("store: write deletion marker for %s: %w", id, err)
		}
	} else if err != nil {
		return fmt.Errorf("store: check deletion marker for %s: %w", id, err)
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

// readDeletionMarkerTenant distinguishes live runs from both legacy and
// tenant-bound tombstones. Legacy markers have just a timestamp line.
func (s *FilesystemRunStore) readDeletionMarkerTenant(id string) (string, bool, error) {
	data, err := os.ReadFile(filepath.Join(s.runDir(id), deletionMarkerName))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read deletion marker: %w", err)
	}
	_, body, _ := bytes.Cut(data, []byte{'\n'})
	var tenant string
	if len(body) != 0 && json.Unmarshal(body, &tenant) != nil {
		return "", false, errors.New("store: invalid deletion marker tenant")
	}
	return tenant, true, nil
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

// The scan is only a candidate list. Take locks in the same order as deletion:
// the scratch stripe before mu, then the stable run metadata lock.
func (s *FilesystemRunStore) pruneDeletionMarker(ctx context.Context, id string, cutoff time.Time) (bool, error) {
	scratchLock, err := s.lockCodemodeScratch(ctx, id)
	if err != nil {
		return false, err
	}
	defer func() { _ = scratchLock.Unlock() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.acquireRunWriteLock(id)
	if err != nil {
		return false, err
	}
	defer func() { _ = lock.Unlock() }()
	dir := s.runDir(id)
	info, err := os.Stat(filepath.Join(dir, deletionMarkerName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: recheck deletion marker %s: %w", id, err)
	}
	if info.ModTime().After(cutoff) {
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
// header even though ordinary loadRunRaw refuses tombstones. Completed
// tombstones can retain only the marker, without run.json.
// Unbound callers (the prune, the CLI, internal deletes) check the format
// only: the strict header parser refuses shapes the format read accepts. A
// context carrying a run data binding is compared with the run it would
// remove, under the same locks (checkBoundDeletionLocked).
func (s *FilesystemRunStore) checkDeletionHeaderLocked(ctx context.Context, id string) error {
	if ctx == nil {
		return ErrInvalidValueFormat
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := os.ReadFile(s.runJSONPath(id))
	_, bound := RunDataBindingFrom(ctx)
	format := LegacyValueFormat
	if err == nil {
		format, err = RunValueFormatFromJSON(ctx, raw)
		if err == nil && bound {
			err = checkBoundDeletionLocked(ctx, id, raw)
		}
	} else if os.IsNotExist(err) {
		err = nil
		if bound {
			err = s.boundDeletionAbsence(id)
		}
	}
	if err != nil {
		return err
	}
	r := &Run{ID: id, FormatVersion: int(format)}
	_, err = CheckRunFormatReplacement(ctx, r, r)
	return err
}

// checkBoundDeletionLocked compares a bound delete with the run.json it would
// remove, as a bound load does (loadRunRawContext): the strict header, the
// carried binding and the tenant it observed. A successor recreated under the
// same ID answers ErrRunDataIdentityChanged.
func checkBoundDeletionLocked(ctx context.Context, id string, raw []byte) error {
	header, err := RunDataHeaderFromJSON(ctx, raw)
	if err != nil {
		return runJSONUndecodable(id, raw, err)
	}
	if err := RequireRunDataStorage(header.Protocol); err != nil {
		return err
	}
	if header.RunID != id {
		return ErrRunDataIdentityChanged
	}
	if err := CheckRunDataBinding(ctx, header); err != nil {
		return err
	}
	return checkRunJSONObservation(ctx, raw)
}

// boundDeletionAbsence answers a bound delete of a run without run.json what a
// bound load answers: the bound run is no longer there to be removed by this
// call, deliberately deleted (ErrRunDeleted) or absent (ErrRunNotFound).
func (s *FilesystemRunStore) boundDeletionAbsence(id string) error {
	if s.runDeleted(id) {
		return fmt.Errorf("store: delete run %s: %w", id, ErrRunDeleted)
	}
	return fmt.Errorf("store: delete run %s: %w", id, ErrRunNotFound)
}

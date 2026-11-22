package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const codemodeScratchFilename = ".codemode-scratch.json"

var _ CodemodeScratchStore = (*FilesystemRunStore)(nil)

// The lock inode lives outside the swept namespace. A fixed set of stripes
// coordinates independent store instances without accumulating per-run locks.
// Deletion and pruning take the stripe before mu; bank operations never take mu.
func (s *FilesystemRunStore) lockCodemodeScratch(ctx context.Context, runID string) (RunLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, ".codemode-scratch-locks")
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("store: create codemode scratch lock directory: %w", err)
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(strings.ToLower(runID)))
	lock := flock.New(filepath.Join(dir, fmt.Sprintf("%02x.lock", hash.Sum32()%64)))
	ok, err := lock.TryLockContext(ctx, 5*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("store: acquire codemode scratch lock: %w", err)
	}
	if !ok {
		return nil, errors.New("store: codemode scratch lock not acquired")
	}
	return lock, nil
}

func (s *FilesystemRunStore) readCodemodeScratchLocked(ctx context.Context, runID string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Check the marker even if run.json is still present after a partial sweep.
	tenant, deleted, err := s.readDeletionMarkerTenant(runID)
	if err != nil {
		return nil, err
	}
	if deleted {
		if !codemodeScratchTenantMatches(ctx, tenant) {
			return nil, ErrRunNotFound
		}
		return nil, ErrRunDeleted
	}
	run, err := s.loadRunRawContext(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.ID != runID || !codemodeScratchTenantMatches(ctx, run.TenantID) {
		return nil, ErrRunNotFound
	}
	f, err := os.Open(filepath.Join(s.runDir(runID), codemodeScratchFilename))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: open codemode scratch bank: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, codemodeScratchMaxEncodedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("store: read codemode scratch bank: %w", err)
	}
	return data, nil
}

// An unstamped run belongs to the local single-tenant filesystem store.
func codemodeScratchTenantMatches(ctx context.Context, tenant string) bool {
	if tenant == "" {
		return true
	}
	if scopedTenant, scoped := TenantFromContext(ctx); scoped {
		return scopedTenant == tenant
	}
	return IsWithoutTenantFilter(ctx)
}

func (s *FilesystemRunStore) PutCodemodeScratch(ctx context.Context, runID, key string, value json.RawMessage, limits CodemodeScratchLimits) error {
	put, err := PrepareCodemodeScratchPut(key, value, limits)
	if err != nil {
		return err
	}
	if err := SanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	lock, err := s.lockCodemodeScratch(ctx, runID)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	runLock, err := s.acquireRunWriteLock(runID)
	if err != nil {
		return err
	}
	defer func() { _ = runLock.Unlock() }()
	data, err := s.readCodemodeScratchLocked(ctx, runID)
	if err != nil {
		return err
	}
	next, err := put.Apply(data)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := WriteFileAtomic(filepath.Join(s.runDir(runID), codemodeScratchFilename), next, filePerm); err != nil {
		return fmt.Errorf("store: persist codemode scratch bank: %w", err)
	}
	return nil
}

func (s *FilesystemRunStore) LoadCodemodeScratch(ctx context.Context, runID, key string) (json.RawMessage, bool, error) {
	if err := ValidateCodemodeScratchKey(key); err != nil {
		return nil, false, err
	}
	if err := SanitizePathComponent("run ID", runID); err != nil {
		return nil, false, err
	}
	lock, err := s.lockCodemodeScratch(ctx, runID)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = lock.Unlock() }()
	runLock, err := s.acquireRunWriteLock(runID)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = runLock.Unlock() }()
	data, err := s.readCodemodeScratchLocked(ctx, runID)
	if err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return LoadCodemodeScratchValue(data, key)
}

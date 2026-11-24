package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Artifacts
// ---------------------------------------------------------------------------

// WriteArtifact persists an artifact for a node at the given version and
// updates the run's artifact index for O(1) latest-version lookups.
func (s *FilesystemRunStore) WriteArtifact(ctx context.Context, a *Artifact) error {
	if err := sanitizePathComponent("run ID", a.RunID); err != nil {
		return err
	}
	if err := sanitizePathComponent("node ID", a.NodeID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.acquireRunWriteLock(a.RunID)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	f, r, err := s.valueRecordFormatLocked(ctx, a.RunID, true)
	if err != nil {
		return err
	}
	if a.WrittenAt.IsZero() {
		a.WrittenAt = time.Now().UTC()
	}
	data, err := marshalValueRecordIndent(ctx, f, ExactArtifact, a)
	if err != nil {
		return fmt.Errorf("store: marshal artifact: %w", err)
	}
	p, err := s.openRunPayloadPath(ctx, a.RunID, true, "artifacts", a.NodeID, fmt.Sprintf("%d.json", a.Version))
	if err != nil {
		return err
	}
	defer p.Close()

	// Hold s.mu across the artifact file write AND the index update so
	// the on-disk file and the cached pointer in run.json land together.
	// Without this, a concurrent LoadRun/SaveRun could observe an index
	// that points to a version not yet on disk (or miss one already
	// written), and a crash between the two writes would leave the
	// artifact orphan to a directory-scan fallback every read.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.WriteAtomic(ctx, data, filePerm); err != nil {
		return err
	}

	// Update the artifact index in run.json. The index is a cache — if it's
	// stale, LoadLatestArtifact falls back to a directory scan — so a fresh
	// run with no run.json yet (loadRunRaw reports ErrRunNotFound) is not
	// fatal. But once run.json exists, a failure to update the index (e.g.
	// ENOSPC, permission denied, JSON encode error) IS surfaced to the
	// caller: a silently dropped index update can cause downstream nodes to
	// read a stale artifact version, which is a correctness bug, not a
	// performance degradation. Callers can decide to retry or fail the run.
	if r == nil {
		return nil
	}
	if r.ArtifactIndex == nil {
		r.ArtifactIndex = make(map[string]int)
	}
	if cur, ok := r.ArtifactIndex[a.NodeID]; !ok || a.Version > cur {
		r.ArtifactIndex[a.NodeID] = a.Version
		if err := s.writeRunLockedContext(ctx, r, false, nil); err != nil {
			return fmt.Errorf("store: write artifact: update index: %w", err)
		}
	}
	return nil
}

// LoadArtifact reads a specific artifact version.
func (s *FilesystemRunStore) LoadArtifact(ctx context.Context, runID, nodeID string, version int) (*Artifact, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}
	lock, err := s.acquireRunWriteLock(runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()
	f, _, err := s.valueRecordFormatLocked(ctx, runID, false)
	if err != nil {
		return nil, err
	}
	p, err := s.openRunPayloadPath(ctx, runID, false, "artifacts", nodeID, fmt.Sprintf("%d.json", version))
	if err != nil {
		return nil, err
	}
	defer p.Close()
	data, err := p.ReadFile()
	if err != nil {
		return nil, fmt.Errorf("store: load artifact: %w", err)
	}
	return decodeArtifactValue(ctx, f, data)
}

// LoadLatestArtifact returns the artifact with the highest version for a node.
// It first checks the run's artifact index for an O(1) lookup and falls back
// to a directory scan for backward compatibility with older run formats.
func (s *FilesystemRunStore) LoadLatestArtifact(ctx context.Context, runID, nodeID string) (*Artifact, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}

	// Fast path: use artifact index if available.
	if r, err := s.LoadRun(ctx, runID); err != nil && !errors.Is(err, ErrRunNotFound) {
		return nil, err
	} else if err == nil && r.ArtifactIndex != nil {
		if v, ok := r.ArtifactIndex[nodeID]; ok {
			return s.LoadArtifact(ctx, runID, nodeID, v)
		}
	}

	// Fallback: directory scan (backward compat with old runs without index).
	lock, err := s.acquireRunWriteLock(runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()
	f, _, err := s.valueRecordFormatLocked(ctx, runID, false)
	if err != nil {
		return nil, err
	}
	dir, err := s.openRunPayloadDirectory(ctx, runID, false, "artifacts", nodeID)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir()
	if err != nil {
		return nil, fmt.Errorf("store: list artifacts: %w", err)
	}
	maxVersion := -1
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		vStr := strings.TrimSuffix(name, ".json")
		v, err := strconv.Atoi(vStr)
		if err != nil {
			continue
		}
		if v > maxVersion {
			maxVersion = v
		}
	}
	if maxVersion < 0 {
		return nil, fmt.Errorf("store: no artifacts for node %s in run %s", nodeID, runID)
	}
	data, err := dir.ReadFile(fmt.Sprintf("%d.json", maxVersion))
	if err != nil {
		return nil, fmt.Errorf("store: load artifact: %w", err)
	}
	return decodeArtifactValue(ctx, f, data)
}

// ArtifactVersionInfo is the lightweight (version, mtime) pair returned by
// ListArtifactVersions — the directory enumeration without the full body
// decode that LoadArtifact incurs.
type ArtifactVersionInfo struct {
	Version   int
	WrittenAt time.Time
}

// ListArtifactVersions enumerates the persisted artifact versions for a
// node in ascending order, returning each version's mtime without
// decoding the body. Returns (nil, nil) when the node has no artifact
// directory (a node that hasn't published anything yet).
func (s *FilesystemRunStore) ListArtifactVersions(ctx context.Context, runID, nodeID string) ([]ArtifactVersionInfo, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}
	lock, err := s.acquireRunWriteLock(runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()
	if _, _, err := s.valueRecordFormatLocked(ctx, runID, false); err != nil {
		return nil, err
	}
	dir, err := s.openRunPayloadDirectory(ctx, runID, false, "artifacts", nodeID)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: list artifact versions: %w", err)
	}
	out := make([]ArtifactVersionInfo, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		v, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, ArtifactVersionInfo{Version: v, WrittenAt: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func decodeArtifactValue(ctx context.Context, f ValueFormat, data []byte) (*Artifact, error) {
	var a Artifact
	if err := DecodeValueRecordJSON(ctx, f, ExactArtifact, data, &a); err != nil {
		return nil, fmt.Errorf("store: decode artifact: %w", err)
	}
	return &a, nil
}

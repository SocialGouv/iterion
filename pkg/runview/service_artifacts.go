package runview

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ListArtifacts enumerates the persisted artifacts for one node by
// reading the artifact directory directly — avoids the O(versions)
// JSON-decode of the full bodies that LoadArtifact would do just to
// extract the version number. Returns the versions in ascending order.
func (s *Service) ListArtifacts(runID, nodeID string) ([]ArtifactSummary, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := validatePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.storeDir, "runs", runID, "artifacts", nodeID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("runview: list artifacts: %w", err)
	}
	out := make([]ArtifactSummary, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		v, parseErr := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if parseErr != nil {
			continue
		}
		info, statErr := e.Info()
		if statErr != nil {
			continue
		}
		out = append(out, ArtifactSummary{Version: v, WrittenAt: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// LoadArtifact returns one persisted artifact body.
//
// Uses context.Background — does NOT carry caller identity. Use
// LoadArtifactCtx from cloud HTTP handlers so the mongo tenant_id
// filter applies (cross-tenant LoadArtifact today leaks bodies).
func (s *Service) LoadArtifact(runID, nodeID string, version int) (*store.Artifact, error) {
	return s.store.LoadArtifact(context.Background(), runID, nodeID, version)
}

// LoadArtifactCtx is the tenant-aware variant of LoadArtifact.
func (s *Service) LoadArtifactCtx(ctx context.Context, runID, nodeID string, version int) (*store.Artifact, error) {
	return s.store.LoadArtifact(ctx, runID, nodeID, version)
}

// ListArtifactFiles enumerates the tool-produced files dropped under
// runs/<id>/artifact_files by in-sandbox tools (write_audit_md,
// emit_sbom, …). Returns nil when the store doesn't satisfy
// RunFilesStore (cloud mode) so the HTTP handler can surface an empty
// list cleanly without leaking the backend choice. Validates the run
// ID before delegating, mirroring ListArtifacts.
func (s *Service) ListArtifactFiles(runID string) ([]store.RunFileInfo, error) {
	return s.ListArtifactFilesCtx(context.Background(), runID)
}

// ListArtifactFilesCtx is the tenant-aware variant of ListArtifactFiles.
func (s *Service) ListArtifactFilesCtx(ctx context.Context, runID string) ([]store.RunFileInfo, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	rfs := store.AsRunFilesStore(s.store)
	if rfs == nil {
		return nil, nil
	}
	return rfs.ListRunFiles(ctx, runID)
}

// OpenArtifactFile streams one tool-produced file from the run's
// artifact_files area. Path-traversal protection lives in
// store.OpenRunFile (caller-side defence); the runview wrapper only
// validates the run-id component and delegates. Returns a nil reader
// when the store doesn't satisfy RunFilesStore.
func (s *Service) OpenArtifactFile(runID, relPath string) (io.ReadCloser, store.RunFileInfo, error) {
	return s.OpenArtifactFileCtx(context.Background(), runID, relPath)
}

// OpenArtifactFileCtx is the tenant-aware variant of OpenArtifactFile.
func (s *Service) OpenArtifactFileCtx(ctx context.Context, runID, relPath string) (io.ReadCloser, store.RunFileInfo, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return nil, store.RunFileInfo{}, err
	}
	rfs := store.AsRunFilesStore(s.store)
	if rfs == nil {
		return nil, store.RunFileInfo{}, fmt.Errorf("runview: artifact files unavailable for this store")
	}
	return rfs.OpenRunFile(ctx, runID, relPath)
}

// ReadToolBlob streams a slice of a tool's stored I/O body (sidecar
// blob written by the hooks layer when the call exceeded the inline
// threshold). offset is the byte offset to start at; limit caps the
// bytes returned (0 = "all from offset"). Returns the bytes read, the
// full blob size, eof when offset+len(data) == total, and an error
// wrapping os.ErrNotExist when the blob doesn't exist.
//
// Returns a clear "unavailable" error when the store doesn't satisfy
// ToolBlobStore (cloud mode today — the hooks layer falls back to
// inline-only persistence in that case, so the studio doesn't issue
// the fetch).
func (s *Service) ReadToolBlob(runID, toolUseID, kind string, offset, limit int64) ([]byte, int64, bool, error) {
	return s.ReadToolBlobCtx(context.Background(), runID, toolUseID, kind, offset, limit)
}

// ReadToolBlobCtx is the tenant-aware variant of ReadToolBlob.
func (s *Service) ReadToolBlobCtx(ctx context.Context, runID, toolUseID, kind string, offset, limit int64) ([]byte, int64, bool, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return nil, 0, false, err
	}
	tbs := store.AsToolBlobStore(s.store)
	if tbs == nil {
		return nil, 0, false, fmt.Errorf("runview: tool blobs unavailable for this store")
	}
	return tbs.ReadToolBlob(ctx, runID, toolUseID, kind, offset, limit)
}

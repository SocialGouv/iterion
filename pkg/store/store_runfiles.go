package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Run files (tool-produced artifact files surfaced via the studio)
// ---------------------------------------------------------------------------

// runFilesDirForFormat selects the retained incarnation's file namespace.
// The historical format suffix remains unchanged for existing callers.
func (s *FilesystemRunStore) runFilesDirForFormat(ctx context.Context, runID string, f ValueFormat) (string, error) {
	return s.runPayloadPath(ctx, runID, runFilesNamespacePart(f))
}
func runFilesNamespacePart(f ValueFormat) string {
	if f == ExactValueFormat {
		return "artifact_files-v2"
	}
	return "artifact_files"
}

// EnsureRunFilesDir satisfies RunFilesStore. Idempotent.
func (s *FilesystemRunStore) EnsureRunFilesDir(ctx context.Context, runID string) (string, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return "", err
	}
	// Tombstone guard BEFORE the MkdirAll below: re-provisioning the
	// scratch dir gets the typed refusal instead of rebuilding a deleted
	// run's tree.
	_, unlock, err := s.lockRunMetadataMutation(ctx, runID)
	if err != nil {
		return "", err
	}
	defer unlock()
	format, _, err := s.valueRecordFormatLocked(ctx, runID, false)
	if err != nil {
		return "", err
	}
	if binding, ok := RunDataBindingFrom(ctx); ok && binding.Header().Protocol == IncarnationRunData {
		grant, err := s.openBoundRunFilesGrantLocked(ctx, runID, format)
		if err != nil {
			return "", err
		}
		defer grant.Close()
		return grant.HostPath(), nil
	}

	dir, err := s.runFilesDirForFormat(ctx, runID, format)
	if err != nil {
		return "", err
	}
	runDir := filepath.Dir(dir)
	if err := os.MkdirAll(runDir, dirPerm); err != nil {
		return "", fmt.Errorf("store: mkdir run dir: %w", err)
	}
	if err := os.Chmod(runDir, dirPerm); err != nil {
		return "", fmt.Errorf("store: chmod run dir: %w", err)
	}

	if err := ensurePlainDirNoSymlink(dir, dirPerm); err != nil {
		return "", fmt.Errorf("store: mkdir run files dir: %w", err)
	}
	// Loosen perms so the in-sandbox container user (devbox, typically
	// uid 1000) can write here even when the host daemon owner is also
	// uid 1000 — explicit 0o775 + setting the dir as group-writable
	// covers the common case and keeps a future "container user is
	// 1001" deployment from silently failing the bind-mount writes.
	_ = os.Chmod(dir, 0o775)
	return dir, nil
}

// ensurePlainDirNoSymlink creates dir but refuses to treat an existing
// symlink as success. os.MkdirAll follows a final symlink to a directory,
// which would let a sandbox-controlled artifact_files entry redirect the
// bind-mount/output area outside the run; replace that stale symlink with a
// real directory instead.
func ensurePlainDirNoSymlink(dir string, perm os.FileMode) error {
	info, err := os.Lstat(dir)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(dir); err != nil {
				return err
			}
		} else {
			if !info.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", dir)
			}
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Mkdir(dir, perm); err != nil {
		if os.IsExist(err) {
			info, lstatErr := os.Lstat(dir)
			if lstatErr != nil {
				return lstatErr
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%s exists as symlink", dir)
			}
			if !info.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", dir)
			}
			return nil
		}
		return err
	}
	return nil
}

// cleanRunFilePath validates a user-supplied artifact file path before the
// openat walk. Reject escapes lexically instead of normalising them away:
// "a/../run.json" must be denied, not collapsed to "run.json".
func cleanRunFilePath(relPath string) ([]string, string, error) {
	slashPath := strings.ReplaceAll(relPath, "\\", "/")
	if slashPath == "" || path.IsAbs(slashPath) {
		return nil, "", fmt.Errorf("store: run file not found")
	}
	for _, part := range strings.Split(slashPath, "/") {
		if part == ".." {
			return nil, "", fmt.Errorf("store: run file not found")
		}
	}
	cleaned := path.Clean(slashPath)
	if cleaned == "." {
		return nil, "", fmt.Errorf("store: run file not found")
	}
	components := strings.Split(cleaned, "/")
	for _, part := range components {
		if part == "" || part == "." || part == ".." {
			return nil, "", fmt.Errorf("store: run file not found")
		}
	}
	return components, cleaned, nil
}

// ListRunFiles satisfies RunFilesStore. Returns a sorted slice (by path)
// for stable output; empty (no error) when no files exist.
func (s *FilesystemRunStore) ListRunFiles(ctx context.Context, runID string) ([]RunFileInfo, error) {
	// Historical filesystem tombstones have no format header. The run ID
	// and context remain checkable before deleted-run compatibility applies.
	if _, err := ValueFormatForWrite(ctx, runID); err != nil {
		return nil, err
	}
	_, unlock, err := s.lockRunMetadataMutation(ctx, runID)
	if errors.Is(err, ErrRunDeleted) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer unlock()
	format, _, err := s.valueRecordFormatLocked(ctx, runID, false)
	if err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	dir, err := s.openRunPayloadDirectory(ctx, runID, false, runFilesNamespacePart(format))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if dir.parent.strict {
		return listRetainedRunFiles(ctx, dir)
	}
	root := dir.parent.key
	var out []RunFileInfo
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return filepath.SkipAll
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, RunFileInfo{
			Path:       filepath.ToSlash(rel),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().UTC(),
		})
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: list run files: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// OpenRunFile satisfies RunFilesStore. Implements path-traversal
// protection by walking from an already-open artifact_files directory fd.
// Absolute paths and any `..` component are rejected lexically, every
// intermediate component is opened with O_DIRECTORY|O_NOFOLLOW, and the
// final file is opened with O_NOFOLLOW before fstat. That openat-style walk
// avoids the EvalSymlinks-then-open TOCTOU gap where a sandbox-controlled
// artifact_files tree could swap an intermediate directory for a symlink
// after validation and trick the server into streaming an arbitrary host file.
func (s *FilesystemRunStore) OpenRunFile(ctx context.Context, runID, relPath string) (io.ReadCloser, RunFileInfo, error) {
	_, unlock, err := s.lockRunMetadataMutation(ctx, runID)
	if err != nil {
		return nil, RunFileInfo{}, err
	}
	defer unlock()
	format, _, err := s.valueRecordFormatLocked(ctx, runID, false)
	if err != nil {
		return nil, RunFileInfo{}, err
	}
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, RunFileInfo{}, err
	}
	components, cleaned, err := cleanRunFilePath(relPath)
	if err != nil {
		return nil, RunFileInfo{}, err
	}
	dir, err := s.openRunPayloadDirectory(ctx, runID, false, runFilesNamespacePart(format))
	if err != nil {
		return nil, RunFileInfo{}, err
	}
	defer dir.Close()
	var f *os.File
	var info fs.FileInfo
	if dir.parent.strict {
		if dir.parent.root == nil {
			err = os.ErrNotExist
		} else {
			f, info, err = openRunFileRetained(dir.parent.root, components)
		}
	} else {
		f, info, err = openRunFileAt(dir.parent.key, components)
	}
	if err != nil {
		return nil, RunFileInfo{}, fmt.Errorf("store: run file not found")
	}
	if err := ctx.Err(); err != nil {
		_ = f.Close()
		return nil, RunFileInfo{}, err
	}
	return f, RunFileInfo{
		Path:       filepath.ToSlash(cleaned),
		Size:       info.Size(),
		ModifiedAt: info.ModTime().UTC(),
	}, nil
}

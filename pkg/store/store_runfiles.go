package store

import (
	"context"
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

// runFilesDir returns the per-run scratch directory where tool-produced
// artifact files live. The path is bind-mounted into the sandbox at
// ITERION_ARTIFACT_FILES_DIR so in-container tools can write files
// without going through the worktree (and committing them into the
// bench repo).
func (s *FilesystemRunStore) runFilesDir(runID string) string {
	return filepath.Join(s.runDir(runID), "artifact_files")
}

// EnsureRunFilesDir satisfies RunFilesStore. Idempotent.
func (s *FilesystemRunStore) EnsureRunFilesDir(_ context.Context, runID string) (string, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return "", err
	}
	// Tombstone guard BEFORE the MkdirAll below: re-provisioning the
	// scratch dir gets the typed refusal instead of rebuilding a deleted
	// run's tree.
	if err := s.guardNotDeleted(runID); err != nil {
		return "", err
	}
	runDir := s.runDir(runID)
	if err := os.MkdirAll(runDir, dirPerm); err != nil {
		return "", fmt.Errorf("store: mkdir run dir: %w", err)
	}
	if err := os.Chmod(runDir, dirPerm); err != nil {
		return "", fmt.Errorf("store: chmod run dir: %w", err)
	}

	dir := s.runFilesDir(runID)
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
func (s *FilesystemRunStore) ListRunFiles(_ context.Context, runID string) ([]RunFileInfo, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	root := s.runFilesDir(runID)
	var out []RunFileInfo
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
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
func (s *FilesystemRunStore) OpenRunFile(_ context.Context, runID, relPath string) (io.ReadCloser, RunFileInfo, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, RunFileInfo{}, err
	}
	components, cleaned, err := cleanRunFilePath(relPath)
	if err != nil {
		return nil, RunFileInfo{}, err
	}
	root := s.runFilesDir(runID)
	f, info, err := openRunFileAt(root, components)
	if err != nil {
		return nil, RunFileInfo{}, fmt.Errorf("store: run file not found")
	}
	return f, RunFileInfo{
		Path:       filepath.ToSlash(cleaned),
		Size:       info.Size(),
		ModifiedAt: info.ModTime().UTC(),
	}, nil
}

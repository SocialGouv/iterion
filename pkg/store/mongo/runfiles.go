package mongo

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The cloud (Mongo) store satisfies the tool-produced-artifact-file seam.
// Its WRITE target and READ source deliberately differ (see the
// RunFilesStore / RunFilesUploader docs in pkg/store/iface.go):
//
//   - EnsureRunFilesDir returns a runner-LOCAL scratch dir bind-mounted
//     into the sandbox, so in-sandbox tools write files exactly as they
//     do on the filesystem backend.
//   - After the run the runner calls UploadRunFiles to copy that scratch
//     tree to S3 under runfiles/<runID>/ (reusing the attachments blob
//     path).
//   - ListRunFiles / OpenRunFile read from S3, so the SERVER pod — which
//     never saw the runner's local disk — serves the artifact-files panel
//     for a finished cloud run.
//
// Before this twin, AsRunFilesStore returned nil for cloud runs, so the
// panel was empty and /artifact-files/{path} 404'd.
var (
	_ store.RunFilesStore    = (*Store)(nil)
	_ store.RunFilesUploader = (*Store)(nil)
)

// runFilesScratchDir returns the runner-local scratch area for a run.
func (s *Store) runFilesScratchDir(runID string) string {
	return filepath.Join(s.runFilesScratch, runID)
}

// EnsureRunFilesDir implements store.RunFilesStore: create + return the
// per-run local scratch dir (the sandbox bind-mount source). Loosens the
// perms like the filesystem store so the in-container user (uid 1000)
// can write into a host-owned mount. Idempotent.
func (s *Store) EnsureRunFilesDir(_ context.Context, runID string) (string, error) {
	if err := store.SanitizePathComponent("run ID", runID); err != nil {
		return "", err
	}
	if s.runFilesScratch == "" {
		return "", fmt.Errorf("store/mongo: run-files scratch dir not configured")
	}
	dir := s.runFilesScratchDir(runID)
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return "", fmt.Errorf("store/mongo: mkdir run files scratch: %w", err)
	}
	_ = os.Chmod(dir, 0o775)
	return dir, nil
}

// UploadRunFiles implements store.RunFilesUploader: walk the run's local
// scratch dir and PUT each file to S3 under runfiles/<runID>/<relPath>.
// Returns the number of files uploaded. A missing scratch dir (the run
// produced nothing) is (0, nil). Called by the runner post-run, on a
// background ctx carrying the run's tenant, so a cancelled run still
// flushes what it produced.
//
// On a clean upload the scratch dir is removed: the durable copy now
// lives in S3 (the read source the server pod serves from), so the
// runner-local tree is redundant. Without this a long-lived runner pod —
// which claims many runs over its lifetime — would accumulate one scratch
// dir per run under <TempDir>/iterion-runfiles forever, since DeleteRun's
// scratch sweep runs on the SERVER store (which has no scratch), never on
// the runner. A failed upload keeps the dir so the bytes aren't lost to a
// transient S3 error.
func (s *Store) UploadRunFiles(ctx context.Context, runID string) (int, error) {
	if err := store.SanitizePathComponent("run ID", runID); err != nil {
		return 0, err
	}
	root := s.runFilesScratchDir(runID)
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("store/mongo: stat run files scratch %s: %w", runID, err)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("store/mongo: run files scratch %s is not a directory", runID)
	}
	var uploaded int
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		// Skip anything that isn't a regular file (symlinks, sockets):
		// the sandbox tree is semi-trusted and a symlink could point
		// outside the scratch area.
		if !d.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		// Run files include potentially large audio/video review outputs. Stream
		// from the scratch file into the blob backend; the attachment byte cap is
		// intentionally unrelated and must not make a checkpoint reference a
		// file that the uploader silently discards.
		file, openErr := os.Open(path)
		if openErr != nil {
			return fmt.Errorf("open %s: %w", rel, openErr)
		}
		fi, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return fmt.Errorf("stat %s: %w", rel, statErr)
		}
		if !fi.Mode().IsRegular() {
			_ = file.Close()
			return nil
		}
		// Advertise exactly fi.Size() bytes and cap the stream at it: a tool
		// still appending to the file between Stat and read (the pre-pause
		// review-media flush can run mid-pass) would otherwise send more bytes
		// than the ContentLength and the S3 PUT would reject the body.
		putErr := s.blob.PutRunFile(ctx, runID, filepath.ToSlash(rel), "", io.LimitReader(file, fi.Size()), fi.Size())
		closeErr := file.Close()
		if putErr != nil {
			return fmt.Errorf("put %s: %w", rel, putErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close %s: %w", rel, closeErr)
		}
		uploaded++
		return nil
	})
	if walkErr != nil {
		return uploaded, fmt.Errorf("store/mongo: upload run files %s: %w", runID, walkErr)
	}
	// Durable copies are in S3 now; drop the redundant runner-local tree
	// so it can't accumulate on a long-lived runner pod. Best-effort — a
	// removal failure must not turn a successful upload into an error.
	_ = os.RemoveAll(root)
	return uploaded, nil
}

// ListRunFiles implements store.RunFilesStore: enumerate the run's
// artifact files from S3, sorted by path. Empty slice (no error) when the
// run produced none.
func (s *Store) ListRunFiles(ctx context.Context, runID string) ([]store.RunFileInfo, error) {
	if err := store.SanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	objs, err := s.blob.ListRunFiles(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list run files %s: %w", runID, err)
	}
	out := make([]store.RunFileInfo, 0, len(objs))
	for _, o := range objs {
		out = append(out, store.RunFileInfo{
			Path:       o.Path,
			Size:       o.Size,
			ModifiedAt: o.ModifiedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// OpenRunFile implements store.RunFilesStore: stream one artifact file
// from S3. Traversal protection lives in blob.RunFileKey (rejects
// absolute paths + `..`/empty segments), so an invalid path and a missing
// object both surface as a clean error the HTTP layer maps to 404.
func (s *Store) OpenRunFile(ctx context.Context, runID, relPath string) (io.ReadCloser, store.RunFileInfo, error) {
	if err := store.SanitizePathComponent("run ID", runID); err != nil {
		return nil, store.RunFileInfo{}, err
	}
	rc, obj, err := s.blob.GetRunFile(ctx, runID, relPath)
	if err != nil {
		return nil, store.RunFileInfo{}, fmt.Errorf("store/mongo: run file not found: %w", err)
	}
	return rc, store.RunFileInfo{
		Path:       obj.Path,
		Size:       obj.Size,
		ModifiedAt: obj.ModifiedAt,
	}, nil
}

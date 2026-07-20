package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// RunFileDiff is the persisted before/after content for one file, mirroring
// gitlib.DiffPayload's wire shape so a cloud fallback serves the studio's
// Monaco DiffEditor identically to the live git path. It is captured by the
// runner while the clone still exists (see PopulateRunDiffs) and read back by
// the server pod once the worktree is gone.
//
// Content lives in one of two places, never both:
//   - inline (Before/After) for a diff whose combined size fits the per-file
//     inline cap and the run's remaining budget, or
//   - a blob (BlobRef names an opaque store key holding the {before,after}
//     JSON) for a larger diff offloaded to S3/disk so the Mongo document stays
//     under the 16 MiB BSON ceiling.
//
// Binary/Oversized mirror gitlib.DiffPayload: no content, a placeholder in the
// UI. Truncated is the persistence-specific signal that the diff-content
// budget was exhausted (or a blob offload failed) so this file's content was
// dropped — distinct from Oversized (a single side over gitlib's read cap).
type RunFileDiff struct {
	Path      string  `json:"path" bson:"path"`
	Before    *string `json:"before,omitempty" bson:"before,omitempty"`
	After     *string `json:"after,omitempty" bson:"after,omitempty"`
	Binary    bool    `json:"binary,omitempty" bson:"binary,omitempty"`
	Oversized bool    `json:"oversized,omitempty" bson:"oversized,omitempty"`
	Truncated bool    `json:"truncated,omitempty" bson:"truncated,omitempty"`
	BlobRef   string  `json:"blob_ref,omitempty" bson:"blob_ref,omitempty"`
}

// Diff-content persistence bounds. The per-file inline cap keeps any single
// diff from dominating the Mongo document; the total budget bounds the whole
// snapshot's inline + offloaded content so a vendor-bump run cannot write an
// unbounded amount. A per-file diff above the inline cap but within the blob
// cap is offloaded; anything past the total budget is dropped (Truncated).
const (
	// perFileInlineDiffBytes is the combined before+after size a file may
	// carry inline in the RunGitMeta document.
	perFileInlineDiffBytes = 128 << 10 // 128 KiB
	// maxInlineRunDiffBytes bounds the CUMULATIVE inline content across a run
	// so the single RunGitMeta document stays well under Mongo's 16 MiB BSON
	// ceiling — the per-file inline cap alone does not, since a run touching
	// thousands of sub-128 KiB files would otherwise pile ~48 MiB of inline
	// content into one document and fail the ReplaceOne (losing the whole
	// snapshot, including the commit/file lists). Once this is exhausted,
	// further content is offloaded to a blob instead of inlined.
	maxInlineRunDiffBytes = 12 << 20 // 12 MiB
	// perFileBlobDiffBytes caps a file offloaded to a blob. gitlib already
	// caps each side at 5 MiB (Oversized past that), so ~12 MiB comfortably
	// covers any two in-range sides that carry content.
	perFileBlobDiffBytes = 12 << 20 // 12 MiB
	// totalRunDiffBytes bounds the whole run's persisted diff content
	// (inline + offloaded). Past this the remaining files are Truncated.
	totalRunDiffBytes = 48 << 20 // 48 MiB
)

// RunDiffBlobStore is the optional seam a store implements to offload large
// per-file diff content out of the RunGitMeta document: the filesystem store
// writes runs/<id>/gitdiffs/<ref>.json, the Mongo store PUTs an attachment
// under attachments/<id>/__gitdiff/<ref> (so DeleteRunAttachments reclaims
// it). Callers MUST nil-check via AsRunDiffBlobStore; a store without the seam
// simply keeps every diff inline or Truncated.
type RunDiffBlobStore interface {
	// PutRunDiffBlob stores body under an opaque, run-scoped ref. Idempotent.
	PutRunDiffBlob(ctx context.Context, runID, ref string, body []byte) error
	// GetRunDiffBlob returns the body previously stored under ref, or an
	// error (wrapping os.ErrNotExist for a missing ref where determinable).
	GetRunDiffBlob(ctx context.Context, runID, ref string) ([]byte, error)
}

// AsRunDiffBlobStore returns s as RunDiffBlobStore when the backend can
// offload diff blobs, or nil otherwise.
func AsRunDiffBlobStore(s RunStore) RunDiffBlobStore {
	if s == nil {
		return nil
	}
	g, _ := s.(RunDiffBlobStore)
	return g
}

// diffBlobRef derives a filesystem-safe, collision-resistant blob ref from a
// scope (e.g. "range" or a commit SHA) and a file path.
func diffBlobRef(scope, path string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + path))
	return hex.EncodeToString(sum[:])
}

// PopulateRunDiffs computes bounded before/after diff content for meta's
// Files (base..head range) and CommitFiles (per commit) and stores it on meta
// — inline for small diffs, offloaded via sink (when non-nil) for large ones,
// Truncated once the total budget is exhausted. repoDir is the still-present
// clone; runID scopes any offloaded blob.
//
// Best-effort throughout: an unreadable file, a non-git repoDir, or a nil
// meta all no-op cleanly. A run without a base..head range (no BaseCommit, or
// base==head) has nothing to diff and returns immediately.
func PopulateRunDiffs(ctx context.Context, runID, repoDir string, meta *RunGitMeta, sink RunDiffBlobStore) {
	if meta == nil || meta.BaseCommit == "" || meta.BaseCommit == meta.HeadCommit {
		return
	}
	b := &diffBudget{remaining: totalRunDiffBytes, inlineRemaining: maxInlineRunDiffBytes}

	// Range diffs (base..head) — one entry per modified file.
	if len(meta.Files) > 0 {
		fd := make(map[string]*RunFileDiff, len(meta.Files))
		for _, f := range meta.Files {
			payload, err := gitlib.DiffBetween(repoDir, meta.BaseCommit, meta.HeadCommit, f.Path)
			if err != nil {
				continue // tolerate a single unreadable file
			}
			fd[f.Path] = b.store(ctx, sink, runID, diffBlobRef("range", f.Path), payload, meta)
		}
		if len(fd) > 0 {
			meta.FileDiffs = fd
		}
	}

	// Per-commit diffs — one entry per file each commit introduced.
	if len(meta.CommitFiles) > 0 {
		cfd := make(map[string]map[string]*RunFileDiff, len(meta.CommitFiles))
		for sha, files := range meta.CommitFiles {
			perPath := make(map[string]*RunFileDiff, len(files))
			for _, f := range files {
				payload, err := gitlib.DiffOfCommit(repoDir, sha, f.Path)
				if err != nil {
					continue
				}
				perPath[f.Path] = b.store(ctx, sink, runID, diffBlobRef(sha, f.Path), payload, meta)
			}
			if len(perPath) > 0 {
				cfd[sha] = perPath
			}
		}
		if len(cfd) > 0 {
			meta.CommitFileDiffs = cfd
		}
	}
}

// diffBudget tracks the shared inline+offload byte budget across a run's
// diffs, deciding per file whether to keep content inline, offload it, or drop
// it (Truncated). remaining bounds the whole run (inline + offloaded);
// inlineRemaining separately bounds the inline slice that lands in the single
// RunGitMeta document so it stays under Mongo's BSON ceiling.
type diffBudget struct {
	remaining       int
	inlineRemaining int
}

// store converts a live gitlib.DiffPayload into a persisted RunFileDiff,
// enforcing the budget. It sets meta.DiffsTruncated when a file's content is
// dropped so the caller's snapshot records the partial capture.
func (b *diffBudget) store(ctx context.Context, sink RunDiffBlobStore, runID, ref string, p gitlib.DiffPayload, meta *RunGitMeta) *RunFileDiff {
	fd := &RunFileDiff{Path: p.Path, Binary: p.Binary, Oversized: p.Oversized}
	if p.Binary || p.Oversized {
		return fd // no content to store; flags carry the placeholder
	}
	size := diffSize(p)
	if size > b.remaining || size > perFileBlobDiffBytes {
		fd.Truncated = true
		meta.DiffsTruncated = true
		return fd
	}
	if size <= perFileInlineDiffBytes && size <= b.inlineRemaining {
		fd.Before = p.Before
		fd.After = p.After
		b.remaining -= size
		b.inlineRemaining -= size
		return fd
	}
	// Too large for the per-file inline cap, or the cumulative inline budget
	// is spent (keeping the RunGitMeta document under Mongo's BSON ceiling):
	// offload the {before,after} JSON to a blob.
	if sink == nil {
		fd.Truncated = true
		meta.DiffsTruncated = true
		return fd
	}
	body, err := json.Marshal(p)
	if err != nil {
		fd.Truncated = true
		meta.DiffsTruncated = true
		return fd
	}
	if err := sink.PutRunDiffBlob(ctx, runID, ref, body); err != nil {
		fd.Truncated = true
		meta.DiffsTruncated = true
		return fd
	}
	fd.BlobRef = ref
	b.remaining -= size
	return fd
}

// diffSize is the combined byte size of a payload's two sides.
func diffSize(p gitlib.DiffPayload) int {
	n := 0
	if p.Before != nil {
		n += len(*p.Before)
	}
	if p.After != nil {
		n += len(*p.After)
	}
	return n
}

// ResolveRunFileDiff turns a persisted RunFileDiff back into the
// gitlib.DiffPayload wire shape the studio consumes. When the content was
// offloaded (BlobRef set) it fetches and decodes the blob via store; a fetch
// failure degrades to an Oversized placeholder rather than erroring. Truncated
// content maps to Oversized on the wire (both mean "too large to show").
func ResolveRunFileDiff(ctx context.Context, store RunDiffBlobStore, runID string, fd *RunFileDiff) gitlib.DiffPayload {
	if fd == nil {
		return gitlib.DiffPayload{}
	}
	if fd.Truncated {
		return gitlib.DiffPayload{Path: fd.Path, Oversized: true}
	}
	if fd.BlobRef != "" {
		if store == nil {
			return gitlib.DiffPayload{Path: fd.Path, Oversized: true}
		}
		body, err := store.GetRunDiffBlob(ctx, runID, fd.BlobRef)
		if err != nil {
			return gitlib.DiffPayload{Path: fd.Path, Oversized: true}
		}
		var p gitlib.DiffPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return gitlib.DiffPayload{Path: fd.Path, Oversized: true}
		}
		return p
	}
	return gitlib.DiffPayload{
		Path:      fd.Path,
		Before:    fd.Before,
		After:     fd.After,
		Binary:    fd.Binary,
		Oversized: fd.Oversized,
	}
}

// diffBlobPath validates runID + ref and returns
// <root>/runs/<runID>/gitdiffs/<ref>.json for the filesystem blob store.
func (s *FilesystemRunStore) diffBlobPath(runID, ref string) (string, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return "", err
	}
	if err := sanitizePathComponent("diff ref", ref); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "runs", runID, "gitdiffs", ref+".json"), nil
}

// PutRunDiffBlob implements RunDiffBlobStore over the filesystem store.
func (s *FilesystemRunStore) PutRunDiffBlob(_ context.Context, runID, ref string, body []byte) error {
	p, err := s.diffBlobPath(runID, ref)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), dirPerm); err != nil {
		return fmt.Errorf("store: mkdir gitdiffs dir: %w", err)
	}
	return writeFileAtomic(p, body, filePerm)
}

// GetRunDiffBlob implements RunDiffBlobStore over the filesystem store.
func (s *FilesystemRunStore) GetRunDiffBlob(_ context.Context, runID, ref string) ([]byte, error) {
	p, err := s.diffBlobPath(runID, ref)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("store: diff blob %s/%s: %w", runID, ref, os.ErrNotExist)
		}
		return nil, fmt.Errorf("store: read diff blob: %w", err)
	}
	return data, nil
}

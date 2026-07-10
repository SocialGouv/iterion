package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// RunGitMeta is the persisted snapshot of a run's git activity — the
// commits it produced (base..head), the per-commit file lists, and the
// modified-files list against the run's baseline. It exists because a
// cloud run's worktree lives in the ephemeral runner pod and vanishes
// with it: the server pod serving GET /api/runs/{id}/commits and /files
// has no repo to inspect, so the runner records this metadata into the
// store (Mongo in cloud, filesystem locally) while the worktree still
// exists, and the handlers fall back to it when the working directory is
// gone. See docs/adr/060-persist-run-git-metadata.md.
//
// It is a whole-snapshot overwrite (not append-only): the runner
// recomputes and re-saves it on each commit-in-stride and again at
// finalize, so the latest write always reflects the full base..head
// range. The types are the same gitlib.CommitInfo / gitlib.FileStatus
// the live git path produces, so the HTTP handlers serve persisted and
// live data through one wire shape with no conversion.
type RunGitMeta struct {
	// BaseCommit is the SHA the run's range is measured from — the
	// worktree/clone HEAD at the moment execution started. Empty only
	// for a degenerate recording.
	BaseCommit string `json:"base_commit,omitempty" bson:"base_commit,omitempty"`
	// HeadCommit is the SHA the worktree HEAD pointed to when the
	// snapshot was taken (equals BaseCommit when the run made no commits).
	HeadCommit string `json:"head_commit,omitempty" bson:"head_commit,omitempty"`
	// Commits are the commits in (BaseCommit, HeadCommit], oldest first
	// (git log --reverse order), exactly as gitlib.Log returns them.
	Commits []gitlib.CommitInfo `json:"commits" bson:"commits"`
	// Files is the modified-files list vs BaseCommit (git diff
	// --name-status BaseCommit..HeadCommit), with diffstat — the payload
	// the studio's Modified-files panel renders in branch mode.
	Files []gitlib.FileStatus `json:"files" bson:"files"`
	// CommitFiles maps each commit's full SHA to the files that commit
	// introduced (git show --name-status), so the per-commit detail
	// endpoint works without a repo. Optional — a metadata-only recording
	// may omit it.
	CommitFiles map[string][]gitlib.FileStatus `json:"commit_files,omitempty" bson:"commit_files,omitempty"`
	// UpdatedAt is when this snapshot was recorded.
	UpdatedAt time.Time `json:"updated_at" bson:"updated_at"`
}

// RunGitMetaStore is an optional interface implemented by stores that
// persist a run's git metadata snapshot (see RunGitMeta). Both the
// filesystem store (runs/<id>/gitmeta.json) and the Mongo store
// (run_gitmeta collection, one doc per run) satisfy it, so the cloud
// server pod can serve commits/files from Mongo after the runner pod's
// worktree is gone.
//
// Persistence is best-effort at the call site: recording git metadata
// must never fail a run. Callers MUST nil-check via AsRunGitMetaStore.
type RunGitMetaStore interface {
	// SaveRunGitMeta persists (overwrites) the run's git metadata
	// snapshot. Idempotent — re-saving replaces the prior snapshot.
	SaveRunGitMeta(ctx context.Context, runID string, meta *RunGitMeta) error
	// LoadRunGitMeta returns the persisted snapshot, or (nil, nil) when
	// none was ever recorded for the run.
	LoadRunGitMeta(ctx context.Context, runID string) (*RunGitMeta, error)
}

// AsRunGitMetaStore returns s as RunGitMetaStore when the backend
// persists run git metadata, or nil otherwise. Both filesystem and
// Mongo stores satisfy it.
func AsRunGitMetaStore(s RunStore) RunGitMetaStore {
	if s == nil {
		return nil
	}
	g, _ := s.(RunGitMetaStore)
	return g
}

// BuildRunGitMeta computes a RunGitMeta snapshot from a live repo/worktree
// at repoDir: the commit list and modified-files diffstat over
// (base, HEAD], plus each commit's introduced file list. It is the shared
// producer for both the cloud runner (which persists the result before its
// pod's worktree vanishes) and any local caller that wants to freeze the
// same view.
//
// base is the run's baseline SHA (the worktree/clone HEAD at start). When
// base is empty the range is unknowable, so a metadata-only snapshot with
// just the current HEAD is returned — the studio renders it as "no commits"
// rather than erroring. When base == HEAD (the run made no commits) the
// lists come back empty, which is the correct "no commits" outcome too.
//
// Errors from the underlying git calls are returned so the caller can log
// and skip persistence; a partial commit-files map (one commit's ShowCommit
// failing) is tolerated — that commit is simply omitted from CommitFiles.
func BuildRunGitMeta(repoDir, base string) (*RunGitMeta, error) {
	head, err := gitlib.RevParseHead(repoDir)
	if err != nil {
		return nil, err
	}
	meta := &RunGitMeta{
		BaseCommit: base,
		HeadCommit: head,
		Commits:    []gitlib.CommitInfo{},
		Files:      []gitlib.FileStatus{},
		UpdatedAt:  time.Now().UTC(),
	}
	if base == "" || base == head {
		// No range to diff: either no baseline recorded, or the run made
		// no commits. Both are the empty "no commits" snapshot.
		return meta, nil
	}
	commits, err := gitlib.Log(repoDir, base, head)
	if err != nil {
		return nil, err
	}
	if commits != nil {
		meta.Commits = commits
	}
	files, err := gitlib.StatusBetween(repoDir, base, head)
	if err != nil {
		return nil, err
	}
	if files != nil {
		meta.Files = files
	}
	if len(commits) > 0 {
		meta.CommitFiles = make(map[string][]gitlib.FileStatus, len(commits))
		for _, c := range commits {
			cf, ferr := gitlib.ShowCommit(repoDir, c.SHA)
			if ferr != nil {
				continue // tolerate a single unreadable commit
			}
			if cf == nil {
				cf = []gitlib.FileStatus{}
			}
			meta.CommitFiles[c.SHA] = cf
		}
	}
	return meta, nil
}

// gitMetaPath validates runID and returns <root>/runs/<runID>/gitmeta.json.
func (s *FilesystemRunStore) gitMetaPath(runID string) (string, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "runs", runID, "gitmeta.json"), nil
}

// SaveRunGitMeta implements RunGitMetaStore over runs/<id>/gitmeta.json —
// a single whole-snapshot file rewritten atomically on each save.
func (s *FilesystemRunStore) SaveRunGitMeta(_ context.Context, runID string, meta *RunGitMeta) error {
	if meta == nil {
		return fmt.Errorf("store: SaveRunGitMeta(%s): nil meta", runID)
	}
	p, err := s.gitMetaPath(runID)
	if err != nil {
		return err
	}
	if meta.UpdatedAt.IsZero() {
		meta.UpdatedAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal git meta: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), dirPerm); err != nil {
		return fmt.Errorf("store: mkdir run dir for git meta: %w", err)
	}
	return writeFileAtomic(p, data, filePerm)
}

// LoadRunGitMeta implements RunGitMetaStore: reads runs/<id>/gitmeta.json,
// returning (nil, nil) when the run never recorded any git metadata.
func (s *FilesystemRunStore) LoadRunGitMeta(_ context.Context, runID string) (*RunGitMeta, error) {
	p, err := s.gitMetaPath(runID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read git meta: %w", err)
	}
	var meta RunGitMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("store: decode git meta: %w", err)
	}
	return &meta, nil
}

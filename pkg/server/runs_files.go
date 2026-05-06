package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

// runFilesResponse is the wire shape of GET /api/runs/{id}/files. The
// `available` flag is the editor's gate for showing the modified-files
// panel: a falsy value paired with a `reason` lets the UI render a
// neutral empty state ("Not a git repository", "No working directory")
// rather than treating absence as an error.
//
// `Live` distinguishes the two source-of-truth modes: true when the file
// list comes from `git status --porcelain` against a still-existing
// worktree (uncommitted state), false when it comes from the historical
// `git diff BaseCommit..FinalCommit` after the worktree has been torn
// down (every entry is already committed on the storage branch). The
// editor uses this to label the panel correctly — without it, M/A/D
// badges over committed files read as "uncommitted modifications" and
// confuse the merge story.
type runFilesResponse struct {
	WorkDir  string `json:"work_dir,omitempty"`
	Worktree bool   `json:"worktree,omitempty"`
	// Live is always serialized (no omitempty): the frontend
	// distinguishes "field absent → legacy backend" from "field present
	// and false → historical-diff mode" to pick the right footer label.
	Live      bool                `json:"live"`
	Files     []gitlib.FileStatus `json:"files"`
	Available bool                `json:"available"`
	Reason    string              `json:"reason,omitempty"`
}

// handleListRunFiles returns the modified files in the run's working
// directory. The handler has two sources of truth depending on lifecycle:
//
//   - **Live worktree** (run is running, paused, or failed-resumable): the
//     worktree directory still exists at run.WorkDir, so we read uncommitted
//     state via `git status --porcelain`. This captures changes that have
//     not yet been committed by the workflow.
//
//   - **Finalized run** (worktree gc'd by the engine cleanup): the worktree
//     directory is gone but the run's commits live on Run.FinalCommit /
//     FinalBranch in the main repo. We synthesize the file list from
//     `git diff --name-status BaseCommit..FinalCommit` against repoRoot.
//
// The endpoint never 5xx's on the expected "no panel" outcomes (missing
// WorkDir, non-git directory, no finalization metadata) — it returns 200
// with `available: false` so the editor can branch in the UI without
// parsing error envelopes.
func (s *Server) handleListRunFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	run, err := s.runs.LoadRun(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	if run.WorkDir == "" {
		s.writeJSONFor(w, r, runFilesResponse{
			Files:     []gitlib.FileStatus{},
			Available: false,
			Reason:    "no_workdir",
		})
		return
	}

	// Live-worktree path: try `git status` against WorkDir first.
	if dirExists(run.WorkDir) {
		files, statusErr := gitlib.Status(run.WorkDir)
		if statusErr == nil {
			if files == nil {
				files = []gitlib.FileStatus{}
			}
			s.writeJSONFor(w, r, runFilesResponse{
				WorkDir:   run.WorkDir,
				Worktree:  run.Worktree,
				Live:      true,
				Files:     files,
				Available: true,
			})
			return
		}
		if !errors.Is(statusErr, gitlib.ErrNotGitRepo) {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "git status: %v", statusErr)
			return
		}
		// Falls through to the historical-diff path on ErrNotGitRepo. A
		// directory that exists but is not a git repository is the same
		// failure mode as a removed worktree from the editor's POV.
	}

	// Finalized-run path: derive the file list from BaseCommit..FinalCommit
	// inside the main repo. Requires both refs and a repo root.
	if files, ok := s.historicalFiles(run); ok {
		s.writeJSONFor(w, r, runFilesResponse{
			WorkDir:   run.WorkDir,
			Worktree:  run.Worktree,
			Live:      false,
			Files:     files,
			Available: true,
		})
		return
	}

	s.writeJSONFor(w, r, runFilesResponse{
		WorkDir:   run.WorkDir,
		Worktree:  run.Worktree,
		Files:     []gitlib.FileStatus{},
		Available: false,
		Reason:    "not_git_repo",
	})
}

// handleGetRunFileDiff returns the Before/After contents of one path inside
// the run's working tree, ready for Monaco's DiffEditor. The selection
// between the live and finalized code paths mirrors handleListRunFiles.
func (s *Server) handleGetRunFileDiff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	path := r.URL.Query().Get("path")
	if err := gitlib.ValidateRelPath(path); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid path: %v", err)
		return
	}
	run, err := s.runs.LoadRun(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	if run.WorkDir == "" {
		s.httpErrorFor(w, r, http.StatusConflict, "run has no working directory recorded")
		return
	}

	// Live-worktree path.
	if dirExists(run.WorkDir) {
		payload, diffErr := gitlib.Diff(run.WorkDir, path)
		if diffErr == nil {
			s.writeJSONFor(w, r, payload)
			return
		}
		if !errors.Is(diffErr, gitlib.ErrNotGitRepo) {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "git diff: %v", diffErr)
			return
		}
		// Fall through on ErrNotGitRepo.
	}

	// Finalized-run path: contents from blob refs in the main repo.
	if base, final, repo, ok := s.historicalRefs(run); ok {
		payload, diffErr := gitlib.DiffBetween(repo, base, final, path)
		if diffErr == nil {
			s.writeJSONFor(w, r, payload)
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "git diff: %v", diffErr)
		return
	}

	s.httpErrorFor(w, r, http.StatusConflict, "working directory is not a git repository")
}

// historicalFiles produces the modified-files list for a run whose worktree
// has been torn down. Returns ok=false when finalization metadata is
// insufficient (no FinalCommit, or no resolvable repo root + baseline).
func (s *Server) historicalFiles(run *store.Run) ([]gitlib.FileStatus, bool) {
	base, final, repo, ok := s.historicalRefs(run)
	if !ok {
		return nil, false
	}
	files, err := gitlib.StatusBetween(repo, base, final)
	if err != nil {
		return nil, false
	}
	if files == nil {
		files = []gitlib.FileStatus{}
	}
	return files, true
}

// historicalRefs resolves the (base, final, repoRoot) triple needed to render
// a historical diff for a finalized run. The resolution chain accommodates
// runs from three eras:
//
//  1. **Current**: Run.RepoRoot + Run.BaseCommit are both persisted by the
//     engine — used as-is.
//  2. **Mid-vintage**: Run.FinalCommit + FinalBranch are present but RepoRoot
//     and BaseCommit predate the field. We try to walk up from Run.WorkDir
//     to a `.git` ancestor, then fall back to the editor's CWD; for the
//     baseline we use `git merge-base FinalCommit HEAD` as a plausible
//     replacement.
//  3. **Legacy**: Run.FinalCommit is also missing — historical diff is not
//     recoverable; ok=false.
func (s *Server) historicalRefs(run *store.Run) (base, final, repo string, ok bool) {
	if run.FinalCommit == "" {
		return "", "", "", false
	}
	final = run.FinalCommit
	repo = run.RepoRoot
	// Persisted RepoRoot can refer to a path that no longer resolves
	// locally — common when the run record was rsync'd between a host
	// and a devcontainer, or the user moved the repo. Cheap-stat the
	// `.git` entry to validate (one syscall, no parent walk) and demote
	// to "" so the fallbacks below get a chance instead of failing with
	// ErrNotGitRepo on a stale absolute path.
	if repo != "" {
		if _, statErr := os.Stat(filepath.Join(repo, ".git")); statErr != nil {
			repo = ""
		}
	}
	if repo == "" {
		// Walk up from work_dir; works when run was launched from a
		// `<repo>/.iterion/worktrees/<id>` layout that still exists locally.
		repo = gitlib.FindRepoRoot(run.WorkDir)
	}
	if repo == "" {
		// Last resort: the editor's own CWD. Reasonable when the editor was
		// launched from inside the same repo that hosts the run's persistent
		// branch — the common single-machine case.
		repo = gitlib.FindRepoRoot(s.cfg.WorkDir)
	}
	if repo == "" {
		return "", "", "", false
	}
	base = run.BaseCommit
	if base == "" {
		base = gitlib.MergeBase(repo, run.FinalCommit, "HEAD")
	}
	if base == "" {
		return "", "", "", false
	}
	return base, final, repo, true
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

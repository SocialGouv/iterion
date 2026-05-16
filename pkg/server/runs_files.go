package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

// fileMode selects the source-of-truth for /api/runs/{id}/files:
//
//   - modeUncommitted (default, live worktree only): `git status` against
//     the worktree — captures changes that have not been committed yet.
//   - modeBranch: range diff between Run.BaseCommit and the current tip
//     (HEAD when the worktree is live, FinalCommit when finalized).
//     Equivalent to "branch vs. source branch" — every entry is a change
//     introduced by a commit during the run.
//
// Once the worktree is gc'd, only modeBranch is meaningful: the
// uncommitted view returns available=false with reason="worktree_gone".
type fileMode string

const (
	modeUncommitted fileMode = "uncommitted"
	modeBranch      fileMode = "branch"
)

// parseFileMode reads the ?mode= query param. Unknown values fall back
// to "" so the handler picks the default based on worktree presence.
func parseFileMode(raw string) fileMode {
	switch fileMode(raw) {
	case modeUncommitted:
		return modeUncommitted
	case modeBranch:
		return modeBranch
	default:
		return ""
	}
}

// runFilesResponse is the wire shape of GET /api/runs/{id}/files. The
// `available` flag is the editor's gate for showing the modified-files
// panel: a falsy value paired with a `reason` lets the UI render a
// neutral empty state ("Not a git repository", "No working directory")
// rather than treating absence as an error.
//
// `Live` distinguishes the two source-of-truth modes: true when the file
// list comes from a still-existing worktree (uncommitted or live branch
// range), false when it comes from the historical post-finalization
// diff. `Mode` reflects the effective view ("uncommitted"|"branch")
// regardless of liveness so the frontend can render the segmented
// control without re-deriving from `Live`.
type runFilesResponse struct {
	WorkDir  string `json:"work_dir,omitempty"`
	Worktree bool   `json:"worktree,omitempty"`
	// Live is always serialized (no omitempty): the frontend
	// distinguishes "field absent → legacy backend" from "field present
	// and false → historical-diff mode" to pick the right footer label.
	Live      bool                `json:"live"`
	Mode      fileMode            `json:"mode"`
	Files     []gitlib.FileStatus `json:"files"`
	Available bool                `json:"available"`
	Reason    string              `json:"reason,omitempty"`
}

// handleListRunFiles returns the modified files in the run's working
// directory. The handler has two sources of truth depending on lifecycle:
//
//   - **Live worktree** (run is running, paused, or failed-resumable): the
//     worktree directory still exists at run.WorkDir. The view depends
//     on `?mode=`:
//
//   - `uncommitted` (default): `git status --porcelain` — changes
//     not yet committed by the workflow.
//
//   - `branch`: `git diff BaseCommit..HEAD` — commits the run has
//     produced so far (excludes uncommitted state).
//
//   - **Finalized run** (worktree gc'd by the engine cleanup): only
//     `mode=branch` is meaningful; we synthesize the file list from
//     `git diff BaseCommit..FinalCommit` against repoRoot. A request
//     with `mode=uncommitted` returns available=false with
//     reason="worktree_gone" so the UI can disable that segment.
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
	requested := parseFileMode(r.URL.Query().Get("mode"))
	run, err := s.runs.LoadRunCtx(r.Context(), id)
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

	// Live-worktree path: branch or uncommitted view against the worktree.
	if dirExists(run.WorkDir) {
		effective := requested
		if effective == "" {
			effective = modeUncommitted
		}
		// `branch` mode without a baseline is unrenderable — the worktree
		// exists but we have no anchor for the range. Surface that as
		// no_baseline so the UI can prompt for a different mode, instead
		// of falling through to historical and returning not_git_repo.
		if effective == modeBranch && run.BaseCommit == "" {
			s.writeJSONFor(w, r, runFilesResponse{
				WorkDir:   run.WorkDir,
				Worktree:  run.Worktree,
				Mode:      modeBranch,
				Files:     []gitlib.FileStatus{},
				Available: false,
				Reason:    "no_baseline",
			})
			return
		}
		files, statusErr := liveFiles(run, effective)
		if statusErr == nil {
			if files == nil {
				files = []gitlib.FileStatus{}
			}
			s.writeJSONFor(w, r, runFilesResponse{
				WorkDir:   run.WorkDir,
				Worktree:  run.Worktree,
				Live:      true,
				Mode:      effective,
				Files:     files,
				Available: true,
			})
			return
		}
		if !errors.Is(statusErr, gitlib.ErrNotGitRepo) {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "git: %v", statusErr)
			return
		}
		// Falls through to the historical-diff path on ErrNotGitRepo. A
		// directory that exists but is not a git repository is the same
		// failure mode as a removed worktree from the editor's POV.
	}

	// Past this point the worktree is gone (or never was a git repo).
	// Uncommitted view is meaningless without a worktree: signal that
	// to the UI so it can disable the segment instead of showing an
	// empty list.
	if requested == modeUncommitted {
		s.writeJSONFor(w, r, runFilesResponse{
			WorkDir:   run.WorkDir,
			Worktree:  run.Worktree,
			Mode:      modeUncommitted,
			Files:     []gitlib.FileStatus{},
			Available: false,
			Reason:    "worktree_gone",
		})
		return
	}

	// Finalized-run path: derive the file list from BaseCommit..FinalCommit
	// inside the main repo. Requires both refs and a repo root.
	if files, ok := s.historicalFiles(run); ok {
		s.writeJSONFor(w, r, runFilesResponse{
			WorkDir:   run.WorkDir,
			Worktree:  run.Worktree,
			Live:      false,
			Mode:      modeBranch,
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

// liveFiles returns the live-worktree file list for the requested mode.
// modeUncommitted reads `git status`; modeBranch ranges BaseCommit..HEAD
// inside the worktree (commits not yet finalized).
func liveFiles(run *store.Run, mode fileMode) ([]gitlib.FileStatus, error) {
	if mode == modeBranch {
		return gitlib.StatusBetween(run.WorkDir, run.BaseCommit, "HEAD")
	}
	return gitlib.Status(run.WorkDir)
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
	requested := parseFileMode(r.URL.Query().Get("mode"))
	run, err := s.runs.LoadRunCtx(r.Context(), id)
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
		effective := requested
		if effective == "" {
			effective = modeUncommitted
		}
		if effective == modeBranch && run.BaseCommit == "" {
			s.httpErrorFor(w, r, http.StatusConflict, "branch diff requires base commit")
			return
		}
		payload, diffErr := liveDiff(run, effective, path)
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

// liveDiff selects between the uncommitted (`git status` family) and the
// branch-range diff for a single file. Mirrors liveFiles.
func liveDiff(run *store.Run, mode fileMode, path string) (gitlib.DiffPayload, error) {
	if mode == modeBranch {
		return gitlib.DiffBetween(run.WorkDir, run.BaseCommit, "HEAD", path)
	}
	return gitlib.Diff(run.WorkDir, path)
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

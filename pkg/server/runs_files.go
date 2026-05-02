package server

import (
	"errors"
	"net/http"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// runFilesResponse is the wire shape of GET /api/runs/{id}/files. The
// `available` flag is the editor's gate for showing the modified-files
// panel: a falsy value paired with a `reason` lets the UI render a
// neutral empty state ("Not a git repository", "No working directory")
// rather than treating absence as an error.
type runFilesResponse struct {
	WorkDir   string              `json:"work_dir,omitempty"`
	Worktree  bool                `json:"worktree,omitempty"`
	Files     []gitlib.FileStatus `json:"files"`
	Available bool                `json:"available"`
	Reason    string              `json:"reason,omitempty"`
}

// handleListRunFiles returns the modified files in the run's working
// directory (worktree or cwd). The endpoint never 5xx's on the two
// expected "no panel" outcomes (missing WorkDir, non-git directory) —
// it returns 200 with `available: false` so the editor can branch in
// the UI without parsing error envelopes.
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
	files, err := gitlib.Status(run.WorkDir)
	if err != nil {
		if errors.Is(err, gitlib.ErrNotGitRepo) {
			s.writeJSONFor(w, r, runFilesResponse{
				WorkDir:   run.WorkDir,
				Worktree:  run.Worktree,
				Files:     []gitlib.FileStatus{},
				Available: false,
				Reason:    "not_git_repo",
			})
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "git status: %v", err)
		return
	}
	if files == nil {
		files = []gitlib.FileStatus{}
	}
	s.writeJSONFor(w, r, runFilesResponse{
		WorkDir:   run.WorkDir,
		Worktree:  run.Worktree,
		Files:     files,
		Available: true,
	})
}

// handleGetRunFileDiff returns the HEAD-side and worktree-side contents
// of a single file inside the run's working directory, ready for
// Monaco's DiffEditor. Path is taken from the `path` query string and
// validated against escape attempts before being handed to git.
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
	payload, err := gitlib.Diff(run.WorkDir, path)
	if err != nil {
		if errors.Is(err, gitlib.ErrNotGitRepo) {
			s.httpErrorFor(w, r, http.StatusConflict, "working directory is not a git repository")
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "git diff: %v", err)
		return
	}
	s.writeJSONFor(w, r, payload)
}

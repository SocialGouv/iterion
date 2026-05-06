package server

import (
	"errors"
	"net/http"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// runCommitsResponse is the wire shape of GET /api/runs/{id}/commits.
// `available` mirrors the runFilesResponse contract: a falsy value paired
// with `reason` lets the editor render an empty-state without parsing
// error envelopes (e.g. legacy runs without BaseCommit, or worktree dirs
// torn down before the commits could be promoted).
//
// `default_squash_message` is the message the deferred-merge endpoint
// would use if the user submitted "Squash and merge" without an
// override. The Commits-tab UI pre-fills its message editor with this
// value so the user sees the proposed message before clicking, and
// toggles into edit mode only when they want to override.
type runCommitsResponse struct {
	Commits              []gitlib.CommitInfo `json:"commits"`
	Count                int                 `json:"count"`
	BaseCommit           string              `json:"base_commit,omitempty"`
	HeadCommit           string              `json:"head_commit,omitempty"`
	DefaultSquashMessage string              `json:"default_squash_message,omitempty"`
	Available            bool                `json:"available"`
	Reason               string              `json:"reason,omitempty"`
}

// handleListRunCommits returns the per-iteration commits the workflow
// produced. Lifecycle branches mirror handleListRunFiles:
//
//   - **Live worktree** (run still has its WorkDir): `git log
//     BaseCommit..HEAD` against the worktree captures every commit the
//     workflow has made so far, including ones not yet promoted.
//
//   - **Finalized run** (worktree gc'd): `git log BaseCommit..FinalCommit`
//     against the main repo replays the same history from the storage
//     branch.
func (s *Server) handleListRunCommits(w http.ResponseWriter, r *http.Request) {
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
	if run.WorkDir == "" || run.BaseCommit == "" {
		s.writeJSONFor(w, r, runCommitsResponse{
			Commits:   []gitlib.CommitInfo{},
			Available: false,
			Reason:    reasonForCommits(run),
		})
		return
	}

	// Live-worktree path: log against HEAD of the worktree directory.
	if dirExists(run.WorkDir) {
		commits, logErr := gitlib.Log(run.WorkDir, run.BaseCommit, "HEAD")
		if logErr == nil {
			head, _ := gitlib.RevParseHead(run.WorkDir)
			s.writeJSONFor(w, r, runCommitsResponse{
				Commits:              commits,
				Count:                len(commits),
				BaseCommit:           run.BaseCommit,
				HeadCommit:           head,
				DefaultSquashMessage: runtime.BuildSquashMessageFromCommits(run.WorkDir, head, runtime.RunDisplayName(run), commits),
				Available:            true,
			})
			return
		}
		if !errors.Is(logErr, gitlib.ErrNotGitRepo) {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "git log: %v", logErr)
			return
		}
		// Fall through on ErrNotGitRepo (worktree dir exists but is no
		// longer a git checkout — same shape as removed worktree).
	}

	// Finalized-run path: log against the persisted storage branch.
	if base, final, repo, ok := s.historicalRefs(run); ok {
		commits, logErr := gitlib.Log(repo, base, final)
		if logErr == nil {
			s.writeJSONFor(w, r, runCommitsResponse{
				Commits:              commits,
				Count:                len(commits),
				BaseCommit:           base,
				HeadCommit:           final,
				DefaultSquashMessage: runtime.BuildSquashMessageFromCommits(repo, final, runtime.RunDisplayName(run), commits),
				Available:            true,
			})
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "git log: %v", logErr)
		return
	}

	s.writeJSONFor(w, r, runCommitsResponse{
		Commits:   []gitlib.CommitInfo{},
		Available: false,
		Reason:    "not_git_repo",
	})
}

// reasonForCommits chooses the empty-state reason for the editor when the
// commits list cannot be produced. Mirrors the runFilesResponse reasons
// so the same i18n labels can be reused.
func reasonForCommits(run *store.Run) string {
	if run.WorkDir == "" {
		return "no_workdir"
	}
	if run.BaseCommit == "" {
		return "no_baseline"
	}
	return "not_git_repo"
}

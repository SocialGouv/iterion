package server

import (
	"errors"
	"net/http"
	"regexp"

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

// runCommitDetailResponse is the wire shape of GET /api/runs/{id}/commits/{sha}.
// Fields shadow the per-commit row (so the editor can re-render the header
// off this payload without holding the listing in memory) and add the file
// list that the commit introduced.
type runCommitDetailResponse struct {
	SHA       string              `json:"sha"`
	Short     string              `json:"short"`
	Parent    string              `json:"parent,omitempty"` // empty for root commits
	Subject   string              `json:"subject,omitempty"`
	Author    string              `json:"author,omitempty"`
	Email     string              `json:"email,omitempty"`
	Date      string              `json:"date,omitempty"` // RFC3339
	Files     []gitlib.FileStatus `json:"files"`
	Available bool                `json:"available"`
	Reason    string              `json:"reason,omitempty"`
}

// shaPattern matches a hex SHA (7..64 chars). We accept both short and
// full SHAs at the route level — git itself resolves abbreviated forms,
// and the in-range guard below validates that whatever shape the user
// passed actually maps to a commit reachable from BaseCommit..HEAD.
var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// handleGetRunCommit returns the metadata + file list for one commit
// reachable in the run's history. The handler enforces two guards:
//
//  1. The SHA must be syntactically valid (hex, 7..64 chars). Rejecting
//     malformed input early keeps the git shell-out from being a search
//     surface for arbitrary refs.
//  2. The SHA must resolve to a commit in the run's range
//     (BaseCommit..HEAD live, BaseCommit..FinalCommit finalized). This
//     prevents the editor from exposing pre-run history or sibling
//     branches by guessing SHAs.
func (s *Server) handleGetRunCommit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rawSHA := r.PathValue("sha")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if !shaPattern.MatchString(rawSHA) {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid commit sha")
		return
	}
	run, err := s.runs.LoadRun(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	repo, info, ok := s.resolveRunCommit(run, rawSHA)
	if !ok {
		s.writeJSONFor(w, r, runCommitDetailResponse{
			Files:     []gitlib.FileStatus{},
			Available: false,
			Reason:    "not_in_range",
		})
		return
	}
	files, err := gitlib.ShowCommit(repo, info.SHA)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "git show: %v", err)
		return
	}
	if files == nil {
		files = []gitlib.FileStatus{}
	}
	parent, _ := gitlib.CommitParent(repo, info.SHA)
	s.writeJSONFor(w, r, runCommitDetailResponse{
		SHA:       info.SHA,
		Short:     info.Short,
		Parent:    parent,
		Subject:   info.Subject,
		Author:    info.Author,
		Email:     info.Email,
		Date:      info.Date.Format("2006-01-02T15:04:05Z07:00"),
		Files:     files,
		Available: true,
	})
}

// handleGetRunCommitFileDiff returns the diff for one file as introduced by
// the commit. Validates the SHA against the run's range identically to the
// detail handler so the editor cannot leak content outside the run.
func (s *Server) handleGetRunCommitFileDiff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rawSHA := r.PathValue("sha")
	path := r.URL.Query().Get("path")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if !shaPattern.MatchString(rawSHA) {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid commit sha")
		return
	}
	if err := gitlib.ValidateRelPath(path); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid path: %v", err)
		return
	}
	run, err := s.runs.LoadRun(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	repo, info, ok := s.resolveRunCommit(run, rawSHA)
	if !ok {
		s.httpErrorFor(w, r, http.StatusNotFound, "commit not in run range")
		return
	}
	payload, err := gitlib.DiffOfCommit(repo, info.SHA, path)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "git diff: %v", err)
		return
	}
	s.writeJSONFor(w, r, payload)
}

// resolveRunCommit picks the right repo (live worktree or main repo) and
// validates that rawSHA resolves to a commit in the run's range. Returns
// ok=false when the SHA is unknown, outside the range, or when the run
// has no recoverable git context. The CommitInfo carries the full SHA so
// callers can use it verbatim with downstream git operations.
func (s *Server) resolveRunCommit(run *store.Run, rawSHA string) (string, gitlib.CommitInfo, bool) {
	if run.WorkDir == "" || run.BaseCommit == "" {
		return "", gitlib.CommitInfo{}, false
	}
	// Live worktree first: the commit may exist there before finalization
	// has moved it onto the persistent branch.
	if dirExists(run.WorkDir) {
		if commits, err := gitlib.Log(run.WorkDir, run.BaseCommit, "HEAD"); err == nil {
			if info, found := findCommit(commits, rawSHA); found {
				return run.WorkDir, info, true
			}
			// `git log` succeeded but the SHA wasn't in range — bail
			// rather than falling through to the main repo, which
			// would re-check the same range with a different baseline
			// fallback. The live answer is authoritative.
			return "", gitlib.CommitInfo{}, false
		}
	}
	// Finalized-run path: range against the persistent branch in the main repo.
	base, final, repo, ok := s.historicalRefs(run)
	if !ok {
		return "", gitlib.CommitInfo{}, false
	}
	commits, err := gitlib.Log(repo, base, final)
	if err != nil {
		return "", gitlib.CommitInfo{}, false
	}
	if info, found := findCommit(commits, rawSHA); found {
		return repo, info, true
	}
	return "", gitlib.CommitInfo{}, false
}

// findCommit matches a user-supplied SHA (full or 7+-char abbreviated)
// against the run's commit listing. Abbreviated forms compare against
// the full SHA prefix; full SHAs match exactly. Case-insensitive — hex
// is hex.
func findCommit(commits []gitlib.CommitInfo, rawSHA string) (gitlib.CommitInfo, bool) {
	if rawSHA == "" {
		return gitlib.CommitInfo{}, false
	}
	for _, c := range commits {
		if equalSHA(c.SHA, rawSHA) {
			return c, true
		}
	}
	return gitlib.CommitInfo{}, false
}

// equalSHA matches full SHA against user input (full or prefix), case-
// insensitively. Used to validate that a /commits/{sha} request refers
// to a commit the run actually owns.
func equalSHA(full, candidate string) bool {
	if len(candidate) == 0 || len(candidate) > len(full) {
		return false
	}
	for i := 0; i < len(candidate); i++ {
		a := full[i]
		b := candidate[i]
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

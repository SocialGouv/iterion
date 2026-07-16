package server

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

// maxRunFileEditBytes caps both the read and write side of the in-run file
// editor. The motivating bug was a 22k-file un-gitignored build cache
// flooding the diff; the editor must never try to stream a multi-megabyte
// blob into Monaco (it would freeze the browser) nor accept an unbounded
// write body. 4 MiB comfortably covers any hand-edited source/.gitignore.
const maxRunFileEditBytes = 4 << 20

// fileMode selects the source-of-truth for /api/runs/{id}/files:
//
//   - modeUncommitted (live worktree only): `git status` against the
//     worktree — captures changes that have not been committed yet.
//   - modeBranch: range diff between Run.BaseCommit and the current tip
//     (HEAD when the worktree is live, FinalCommit when finalized).
//     Equivalent to "branch vs. source branch" — every entry is a change
//     introduced by a commit during the run.
//   - modeCombined (live worktree only): the union of modeBranch and
//     modeUncommitted — every file the run has touched, committed or not,
//     each tagged with a `lifecycle` ("committed" | "uncommitted") so the
//     studio can render a subtle committed-vs-in-flight distinction. This
//     is the studio's default while a run is in progress.
//   - modeProduced: "the best available full picture", lifecycle-agnostic.
//     Resolves to combined while the worktree is live, then falls back to
//     the persisted git-metadata snapshot and finally the historical
//     BaseCommit..FinalCommit diff — instead of dead-ending on
//     worktree_gone the way a strict combined request does. Used by the
//     pipeline board's Produced-elements panel, which must keep showing a
//     finished run's files after the engine gc'd its worktree.
//
// Once the worktree is gc'd, only modeBranch and modeProduced are
// meaningful: the uncommitted and combined views return available=false
// with reason="worktree_gone".
type fileMode string

const (
	modeUncommitted fileMode = "uncommitted"
	modeBranch      fileMode = "branch"
	modeCombined    fileMode = "combined"
	modeProduced    fileMode = "produced"
)

// Lifecycle tags annotate combined-mode entries (see combinedFiles). They
// surface on gitlib.FileStatus.Lifecycle for the studio's per-file tint.
const (
	lifecycleCommitted   = "committed"
	lifecycleUncommitted = "uncommitted"
)

// parseFileMode reads the ?mode= query param. Unknown values fall back
// to "" so the handler picks the default based on worktree presence.
func parseFileMode(raw string) fileMode {
	switch fileMode(raw) {
	case modeUncommitted:
		return modeUncommitted
	case modeBranch:
		return modeBranch
	case modeCombined:
		return modeCombined
	case modeProduced:
		return modeProduced
	default:
		return ""
	}
}

// runFilesResponse is the wire shape of GET /api/runs/{id}/files. The
// `available` flag is the studio's gate for showing the modified-files
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
//   - `combined`: the union of the two above, each file tagged with a
//     lifecycle (see combinedFiles). The studio's default while in flight.
//
//   - **Finalized run** (worktree gc'd by the engine cleanup): only
//     `mode=branch` is meaningful; we synthesize the file list from
//     `git diff BaseCommit..FinalCommit` against repoRoot. A request
//     with `mode=uncommitted` or `mode=combined` returns available=false
//     with reason="worktree_gone" so the UI can disable that segment.
//
// The endpoint never 5xx's on the expected "no panel" outcomes (missing
// WorkDir, non-git directory, no finalization metadata) — it returns 200
// with `available: false` so the studio can branch in the UI without
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
		// While the working directory exists, `produced` IS the combined
		// view; the gone-worktree fallbacks below are what set it apart.
		if effective == modeProduced {
			effective = modeCombined
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
		// failure mode as a removed worktree from the studio's POV.
	}

	// Persisted-metadata fallback: a cloud run's worktree lives in the
	// runner pod and is gone by the time this server pod serves the panel.
	// The runner recorded the branch-range modified-files list into the
	// store before the clone was wiped (runner.recordRunGitMeta); serve it.
	// The persisted view is the committed branch range (files vs base) — it
	// cannot reconstruct uncommitted state, so a strict `uncommitted`
	// request still degrades to worktree_gone below; every other mode
	// (default, branch, combined) is answerable from the snapshot.
	if requested != modeUncommitted {
		if s.servePersistedFiles(w, r, run, requested) {
			return
		}
	}

	// Past this point the worktree is gone (or never was a git repo).
	// The uncommitted and combined views both need a worktree to read
	// pending changes: signal that to the UI so it can disable the
	// segment (and auto-fall-back to branch) instead of showing an empty
	// list.
	if requested == modeUncommitted || requested == modeCombined {
		s.writeJSONFor(w, r, runFilesResponse{
			WorkDir:   run.WorkDir,
			Worktree:  run.Worktree,
			Mode:      requested,
			Files:     []gitlib.FileStatus{},
			Available: false,
			Reason:    unavailableReason(run, "worktree_gone"),
		})
		return
	}

	// Finalized-run path: derive the file list from BaseCommit..FinalCommit
	// inside the main repo. Requires both refs and a repo root.
	if files, ok := s.historicalFiles(run); ok {
		// A `produced` request tags the historical rows committed so the
		// studio's per-row diff targets the branch range (the worktree —
		// and with it any uncommitted view — is gone).
		if requested == modeProduced {
			for i := range files {
				files[i].Lifecycle = lifecycleCommitted
			}
		}
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
		Reason:    unavailableReason(run, "not_git_repo"),
	})
}

// unavailableReason picks the reason for a files listing that couldn't be
// resolved. A cloud run's worktree lives on the runner pod and its
// branch-range gitmeta is only recorded at finalize, so while the run is
// still ongoing this server pod has neither the worktree nor a snapshot —
// the panel would otherwise render "not a git repository" / "worktree
// cleaned up", both of which misdescribe a run that simply hasn't produced
// its files yet. For a non-terminal run we return "building" so the studio
// shows a "available when the run finishes" empty state instead. Terminal
// runs fall back to the caller's concrete reason (the worktree really is
// gone).
func unavailableReason(run *store.Run, terminalReason string) string {
	// Only "building" when the worktree directory is genuinely absent from
	// this pod (the cloud runner owns it) — a present-but-non-git directory
	// is a real not_git_repo even mid-run, not a not-yet-produced listing.
	if !run.Status.IsTerminal() && !dirExists(run.WorkDir) {
		return "building"
	}
	return terminalReason
}

// servePersistedFiles serves the modified-files list from the run's
// persisted git-metadata snapshot (RunGitMetaStore). Returns true when it
// handled the response. This is the cloud fallback for a run whose worktree
// is gone: the runner recorded the branch-range diff (files vs base) into
// the store. The snapshot represents the committed branch view, so for a
// `combined` (or `produced`) request the entries are tagged
// lifecycle=committed (there is no uncommitted set to merge). A snapshot
// with zero files is a valid "no changes" answer (Available=true), not an
// empty-state error.
func (s *Server) servePersistedFiles(w http.ResponseWriter, r *http.Request, run *store.Run, requested fileMode) bool {
	gs := store.AsRunGitMetaStore(s.runs.RunStore())
	if gs == nil {
		return false
	}
	meta, err := gs.LoadRunGitMeta(r.Context(), run.ID)
	if err != nil {
		s.logger.Warn("run %s: load git metadata (files): %v", run.ID, err)
		return false
	}
	if meta == nil {
		return false
	}
	files := meta.Files
	if files == nil {
		files = []gitlib.FileStatus{}
	}
	effective := modeBranch
	if requested == modeCombined || requested == modeProduced {
		effective = modeCombined
		tagged := make([]gitlib.FileStatus, len(files))
		for i, f := range files {
			f.Lifecycle = lifecycleCommitted
			tagged[i] = f
		}
		files = tagged
	}
	s.writeJSONFor(w, r, runFilesResponse{
		WorkDir:   run.WorkDir,
		Worktree:  run.Worktree,
		Live:      false,
		Mode:      effective,
		Files:     files,
		Available: true,
	})
	return true
}

// liveFiles returns the live-worktree file list for the requested mode.
// modeUncommitted reads `git status`; modeBranch ranges BaseCommit..HEAD
// inside the worktree (commits not yet finalized); modeCombined merges
// the two via combinedFiles.
func liveFiles(run *store.Run, mode fileMode) ([]gitlib.FileStatus, error) {
	switch mode {
	case modeBranch:
		return gitlib.StatusBetween(run.WorkDir, run.BaseCommit, "HEAD")
	case modeCombined:
		return combinedFiles(run)
	default:
		return gitlib.Status(run.WorkDir)
	}
}

// combinedFiles produces the union of the uncommitted working-tree changes
// (`git status`) and the committed range (BaseCommit..HEAD) for a live
// worktree, tagging each entry with a lifecycle so the studio can paint a
// subtle committed-vs-in-flight distinction.
//
// On a path collision — a file committed during the run AND then re-edited
// (or committed but with further staged/unstaged delta) — the uncommitted
// entry wins: the file is the operator's in-flight concern, and its
// +N|-N reflects the still-pending delta (HEAD..worktree) rather than the
// committed delta. This is a deliberate 2-state simplification matching the
// "committed vs uncommitted" framing; we don't synthesize a third "mixed"
// state or a base..worktree line-count (which would need an extra diff).
//
// When the run has no BaseCommit (e.g. a non-worktree run, or a worktree
// run whose baseline wasn't recorded) the committed range is unknowable, so
// the result degrades gracefully to the uncommitted set alone.
func combinedFiles(run *store.Run) ([]gitlib.FileStatus, error) {
	// The committed range and the uncommitted status are independent git
	// scans, so run the range concurrently with the status read (each
	// already parallelizes its own name-status + numstat internally). Mirrors
	// the goroutine+channel idiom in pkg/git/status.go so combined — the
	// in-flight default — pays max(scan) latency, not the sum.
	type rangeResult struct {
		files []gitlib.FileStatus
		err   error
	}
	var rangeCh chan rangeResult
	if run.BaseCommit != "" {
		rangeCh = make(chan rangeResult, 1)
		go func() {
			files, err := gitlib.StatusBetween(run.WorkDir, run.BaseCommit, "HEAD")
			rangeCh <- rangeResult{files, err}
		}()
	}

	uncommitted, err := gitlib.Status(run.WorkDir)
	if err != nil {
		if rangeCh != nil {
			<-rangeCh
		}
		return nil, err
	}
	seen := make(map[string]struct{}, len(uncommitted))
	out := make([]gitlib.FileStatus, 0, len(uncommitted))
	for _, f := range uncommitted {
		f.Lifecycle = lifecycleUncommitted
		out = append(out, f)
		seen[f.Path] = struct{}{}
	}
	if rangeCh != nil {
		res := <-rangeCh
		if res.err != nil {
			return nil, res.err
		}
		for _, f := range res.files {
			if _, dup := seen[f.Path]; dup {
				continue
			}
			f.Lifecycle = lifecycleCommitted
			out = append(out, f)
		}
	}
	// Deterministic order for test stability; the studio re-sorts into a
	// tree (buildFileTree) so display order is unaffected either way.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
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

	// Live-worktree path.
	if run.WorkDir != "" && dirExists(run.WorkDir) {
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

	// Persisted-metadata fallback: a cloud run's worktree lives in the runner
	// pod and is gone by the time this server pod serves the diff. The runner
	// recorded per-file before/after content into the store (PopulateRunDiffs);
	// serve it. The snapshot is the committed base..head range, so a strict
	// `uncommitted` request has no persisted answer and falls through to 409.
	if requested != modeUncommitted && s.servePersistedFileDiff(w, r, run, path) {
		return
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

	if run.WorkDir == "" {
		s.httpErrorFor(w, r, http.StatusConflict, "run has no working directory recorded")
		return
	}
	s.httpErrorFor(w, r, http.StatusConflict, "working directory is not a git repository")
}

// servePersistedFileDiff serves one file's base..head diff from the run's
// persisted git-metadata snapshot. Returns true when it handled the response.
// This is the cloud fallback for a diff click on a run whose worktree is gone:
// the runner recorded the before/after content (inline or blob-offloaded) into
// the store. A path with no persisted diff returns false so the caller can try
// the historical path / 409, keeping the "no answer" contract explicit.
func (s *Server) servePersistedFileDiff(w http.ResponseWriter, r *http.Request, run *store.Run, path string) bool {
	gs := store.AsRunGitMetaStore(s.runs.RunStore())
	if gs == nil {
		return false
	}
	meta, err := gs.LoadRunGitMeta(r.Context(), run.ID)
	if err != nil {
		s.logger.Warn("run %s: load git metadata (file diff): %v", run.ID, err)
		return false
	}
	if meta == nil {
		return false
	}
	fd := meta.FileDiffs[path]
	if fd == nil {
		return false
	}
	bs := store.AsRunDiffBlobStore(s.runs.RunStore())
	payload := store.ResolveRunFileDiff(r.Context(), bs, run.ID, fd)
	s.writeJSONFor(w, r, payload)
	return true
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
//     to a `.git` ancestor, then fall back to the studio's CWD; for the
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
		// Last resort: the studio's own CWD. Reasonable when the studio was
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

// runFileContentResponse is the wire shape of
// GET /api/runs/{id}/files/content and the success body of the PUT. Content
// is empty for binary files (Binary=true) and for a not-yet-existing path
// (Exists=false); the studio uses Exists to seed a fresh editor buffer
// (e.g. creating a `.gitignore` that doesn't exist yet) rather than erroring.
type runFileContentResponse struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Binary  bool   `json:"binary"`
	Exists  bool   `json:"exists"`
}

// saveRunFileRequest is the body of PUT /api/runs/{id}/files/content.
type saveRunFileRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// handleGetRunFileContent returns the raw UTF-8 contents of one path inside a
// run's live worktree, ready to seed an editable Monaco buffer. Unlike
// /files/diff (which returns before/after for changed files only), this reads
// any path under the worktree — including an unchanged or not-yet-created
// `.gitignore` — so the operator can fix a stray build/cache dir inline.
//
// Editing is scoped to LIVE worktrees: a finalized/gc'd run has no on-disk
// worktree, so we 409 rather than guessing at the persistent branch. The
// ?path= input is validated by gitlib.ValidateRelPath AND re-resolved through
// safePathWithin (symlink-aware containment) before any FS access — the path
// can never escape run.WorkDir.
func (s *Server) handleGetRunFileContent(w http.ResponseWriter, r *http.Request) {
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
	run, err := s.runs.LoadRunCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	abs, ok := s.resolveRunWorktreePath(w, r, run, path)
	if !ok {
		return
	}
	// #nosec G304 — abs is the output of safePathWithin (symlink-aware
	// containment against run.WorkDir); request input cannot escape the
	// worktree. Mirrors the dirExists nosec rationale on this surface.
	info, statErr := os.Stat(abs)
	if errors.Is(statErr, os.ErrNotExist) {
		s.writeJSONFor(w, r, runFileContentResponse{Path: path, Exists: false})
		return
	}
	if statErr != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "stat: %v", statErr)
		return
	}
	if info.IsDir() {
		s.httpErrorFor(w, r, http.StatusBadRequest, "path is a directory")
		return
	}
	if info.Size() > maxRunFileEditBytes {
		s.httpErrorFor(w, r, http.StatusRequestEntityTooLarge,
			"file too large to edit (%d bytes; limit %d)", info.Size(), maxRunFileEditBytes)
		return
	}
	// #nosec G304 — see stat rationale above; abs is containment-checked.
	data, readErr := os.ReadFile(abs)
	if readErr != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "read: %v", readErr)
		return
	}
	resp := runFileContentResponse{Path: path, Exists: true}
	if bytes.IndexByte(data, 0) >= 0 {
		resp.Binary = true // leave Content empty; the editor refuses binary
	} else {
		resp.Content = string(data)
	}
	s.writeJSONFor(w, r, resp)
}

// handleSaveRunFileContent writes operator-edited content back into a run's
// live worktree. Same path-traversal boundary as the GET; additionally
// gated by requireSafeOrigin + rejectCrossStoreWrite (the mutation guards
// every write endpoint on this server carries).
func (s *Server) handleSaveRunFileContent(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	var req saveRunFileRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if err := gitlib.ValidateRelPath(req.Path); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid path: %v", err)
		return
	}
	if len(req.Content) > maxRunFileEditBytes {
		s.httpErrorFor(w, r, http.StatusRequestEntityTooLarge,
			"content too large (%d bytes; limit %d)", len(req.Content), maxRunFileEditBytes)
		return
	}
	run, err := s.runs.LoadRunCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	abs, ok := s.resolveRunWorktreePath(w, r, run, req.Path)
	if !ok {
		return
	}
	// #nosec G304 — abs is the output of safePathWithin (symlink-aware
	// containment against run.WorkDir); request input cannot escape the
	// worktree.
	if info, statErr := os.Stat(abs); statErr == nil && info.IsDir() {
		s.httpErrorFor(w, r, http.StatusBadRequest, "path is a directory")
		return
	}
	// #nosec G304 — see rationale above; abs is containment-checked.
	if err := os.WriteFile(abs, []byte(req.Content), 0o644); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "write: %v", err)
		return
	}
	s.writeJSONFor(w, r, runFileContentResponse{Path: req.Path, Content: req.Content, Exists: true})
}

// resolveRunWorktreePath validates that the run has a live on-disk worktree
// and returns the containment-checked absolute path for relPath. It writes
// the appropriate HTTP error and returns ok=false on any failure, so callers
// can early-return. relPath MUST already have passed gitlib.ValidateRelPath.
func (s *Server) resolveRunWorktreePath(w http.ResponseWriter, r *http.Request, run *store.Run, relPath string) (string, bool) {
	if run.WorkDir == "" {
		s.httpErrorFor(w, r, http.StatusConflict, "run has no working directory recorded")
		return "", false
	}
	if !dirExists(run.WorkDir) {
		s.httpErrorFor(w, r, http.StatusConflict, "run worktree is no longer available; files can only be edited while the worktree exists")
		return "", false
	}
	abs, err := safePathWithin(run.WorkDir, relPath)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid path: %v", err)
		return "", false
	}
	return abs, true
}

func dirExists(p string) bool {
	// #nosec G304 G703 — p is run.WorkDir read from persisted run-state, never raw
	// request input. The only request-borne path on this surface (?path=) is
	// validated by gitlib.ValidateRelPath before any git/FS access.
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

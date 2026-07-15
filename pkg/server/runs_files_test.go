package server

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

// seedRunWithWorkDir creates a run row whose run.json points its
// WorkDir at the provided directory so the /files endpoint has a real
// path to invoke git against.
func seedRunWithWorkDir(t *testing.T, srv *Server, runID, workDir string, worktree bool) {
	t.Helper()
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	r, err := st.CreateRun(context.Background(), runID, "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r.WorkDir = workDir
	r.Worktree = worktree
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
}

// initRepo prepares a fresh git repository with one committed file.
// Reused across the file-endpoint tests for a stable "modified" baseline.
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// TestRunFileDiff_RejectsTraversal confirms the path-traversal guard on the
// file-diff surface: the request-borne ?path= is validated by
// gitlib.ValidateRelPath before any git/FS access, so escape attempts are
// refused with 400 (this is the real external surface behind the
// runs_files.go:dirExists #nosec justification).
func TestRunFileDiff_RejectsTraversal(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	seedRunWithWorkDir(t, srv, "trav", dir, true)

	for _, bad := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"..",
		"-rf",
		"", // empty path
	} {
		t.Run(bad, func(t *testing.T) {
			u := hs.URL + "/api/runs/trav/files/diff?path=" + url.QueryEscape(bad)
			resp, err := http.Get(u)
			if err != nil {
				t.Fatalf("GET %s: %v", u, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("path %q: status = %d, want 400", bad, resp.StatusCode)
			}
		})
	}
}

func TestRunFiles_NoWorkDir(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "no-wd", "wf", store.RunStatusFinished)

	resp, err := http.Get(hs.URL + "/api/runs/no-wd/files")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if out.Available {
		t.Errorf("Available should be false")
	}
	if out.Reason != "no_workdir" {
		t.Errorf("Reason: %q", out.Reason)
	}
}

func TestRunFiles_NotGitRepo(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := t.TempDir()
	seedRunWithWorkDir(t, srv, "not-git", dir, false)

	resp, err := http.Get(hs.URL + "/api/runs/not-git/files")
	if err != nil {
		t.Fatal(err)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if out.Available {
		t.Errorf("Available should be false")
	}
	if out.Reason != "not_git_repo" {
		t.Errorf("Reason: %q", out.Reason)
	}
	if out.WorkDir != dir {
		t.Errorf("WorkDir: %q", out.WorkDir)
	}
}

func TestRunFiles_HappyPath(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedRunWithWorkDir(t, srv, "rich", dir, true)

	resp, err := http.Get(hs.URL + "/api/runs/rich/files")
	if err != nil {
		t.Fatal(err)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if !out.Available {
		t.Fatalf("Available: false (reason=%q)", out.Reason)
	}
	if !out.Worktree {
		t.Errorf("Worktree should be true")
	}
	got := map[string]string{}
	for _, f := range out.Files {
		got[f.Path] = f.Status
	}
	if got["a.txt"] != "M" || got["b.txt"] != "??" {
		t.Errorf("statuses: %+v", got)
	}
}

func TestRunFiles_HappyPath_LiveTrue(t *testing.T) {
	// Live worktree path must mark the response Live=true so the studio
	// labels the panel "Working tree (worktree)" rather than
	// "Committed in this run".
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedRunWithWorkDir(t, srv, "live", dir, true)

	resp, err := http.Get(hs.URL + "/api/runs/live/files")
	if err != nil {
		t.Fatal(err)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if !out.Available || !out.Live {
		t.Errorf("Available=%v Live=%v, want both true", out.Available, out.Live)
	}
}

func TestRunFiles_Historical_RepoRootStaleFallsBackToCWD(t *testing.T) {
	// Regression for run_1778021294883: when run.RepoRoot was persisted
	// pointing at a host path that no longer resolves locally (devcontainer
	// rsync, repo move…), historicalRefs used to short-circuit on the stale
	// absolute path and never try the cfg.WorkDir fallback. Result: the
	// finalized-run files panel returned `available=false reason=not_git_repo`
	// even though the storage branch was reachable from the studio's CWD.
	srv, hs := newTestServer(t)

	// Initialise a real repo at the studio's WorkDir (cfg.WorkDir) and put
	// one commit there — the historical-files endpoint will diff against it.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = srv.cfg.WorkDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(srv.cfg.WorkDir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-q", "-m", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = srv.cfg.WorkDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	baseSHA := revParse(t, srv.cfg.WorkDir, "HEAD")
	if err := os.WriteFile(filepath.Join(srv.cfg.WorkDir, "b.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "b.txt"}, {"commit", "-q", "-m", "add b"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = srv.cfg.WorkDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	finalSHA := revParse(t, srv.cfg.WorkDir, "HEAD")

	// Seed a run with RepoRoot pointing at a non-existent host path (the
	// stale-pointer condition we're regressing) and a WorkDir that's also
	// gone. Only the BaseCommit + FinalCommit + cfg.WorkDir give us a path
	// back to the diff.
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.CreateRun(context.Background(), "hist", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.WorkDir = "/nonexistent/host/path/.iterion/worktrees/hist"
	r.RepoRoot = "/nonexistent/host/path"
	r.Worktree = true
	r.BaseCommit = baseSHA
	r.FinalCommit = finalSHA
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(hs.URL + "/api/runs/hist/files")
	if err != nil {
		t.Fatal(err)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if !out.Available {
		t.Fatalf("Available=false reason=%q, want true (historical fallback to cfg.WorkDir)", out.Reason)
	}
	if out.Live {
		t.Errorf("Live=true, want false (historical-diff path)")
	}
	got := map[string]string{}
	for _, f := range out.Files {
		got[f.Path] = f.Status
	}
	if got["b.txt"] != "A" {
		t.Errorf("expected b.txt status A in BaseCommit..FinalCommit diff, got %+v", got)
	}
}

func revParse(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", ref, err)
	}
	return string(out[:len(out)-1]) // strip trailing newline
}

// TestRunFiles_ModeBranch verifies that mode=branch on a live worktree
// returns committed-during-the-run changes (BaseCommit..HEAD) and skips
// uncommitted worktree state. The fixture commits a.txt + b.txt onto the
// run worktree but leaves c.txt uncommitted: only a.txt + b.txt should
// appear in branch mode.
func TestRunFiles_ModeBranch(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	baseSHA := revParse(t, dir, "HEAD")

	// One committed change inside the "run".
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "b.txt"}, {"commit", "-q", "-m", "add b"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// One uncommitted change that must be excluded in branch mode.
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.CreateRun(context.Background(), "branch-mode", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.WorkDir = dir
	r.Worktree = true
	r.BaseCommit = baseSHA
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(hs.URL + "/api/runs/branch-mode/files?mode=branch")
	if err != nil {
		t.Fatal(err)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if !out.Available || !out.Live {
		t.Fatalf("Available=%v Live=%v reason=%q", out.Available, out.Live, out.Reason)
	}
	if out.Mode != modeBranch {
		t.Errorf("Mode: want branch, got %q", out.Mode)
	}
	got := map[string]string{}
	for _, f := range out.Files {
		got[f.Path] = f.Status
	}
	if got["b.txt"] != "A" {
		t.Errorf("b.txt should be A in branch range, got %+v", got)
	}
	if _, has := got["c.txt"]; has {
		t.Errorf("c.txt (uncommitted) leaked into branch mode: %+v", got)
	}
}

// TestRunFiles_ModeBranchNoBaseline returns no_baseline when mode=branch
// is requested but the run has no BaseCommit recorded — the worktree
// exists but the range is unanchored.
func TestRunFiles_ModeBranchNoBaseline(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	seedRunWithWorkDir(t, srv, "no-base", dir, true)

	resp, err := http.Get(hs.URL + "/api/runs/no-base/files?mode=branch")
	if err != nil {
		t.Fatal(err)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if out.Available {
		t.Errorf("Available should be false")
	}
	if out.Reason != "no_baseline" {
		t.Errorf("Reason: want no_baseline, got %q", out.Reason)
	}
}

// TestRunFiles_ModeUncommittedAfterFinalization returns worktree_gone
// when the worktree directory is missing and mode=uncommitted is
// requested — the UI uses this to disable the segment.
func TestRunFiles_ModeUncommittedAfterFinalization(t *testing.T) {
	srv, hs := newTestServer(t)
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.CreateRun(context.Background(), "wt-gone", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.WorkDir = "/nonexistent/host/worktree"
	r.RepoRoot = "/nonexistent/host"
	r.Worktree = true
	r.Status = store.RunStatusFinished
	r.BaseCommit = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	r.FinalCommit = "feedfacefeedfacefeedfacefeedfacefeedface"
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(hs.URL + "/api/runs/wt-gone/files?mode=uncommitted")
	if err != nil {
		t.Fatal(err)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if out.Available {
		t.Errorf("Available should be false")
	}
	if out.Reason != "worktree_gone" {
		t.Errorf("Reason: want worktree_gone, got %q", out.Reason)
	}
}

// TestRunFiles_BuildingWhileRunning covers the live cloud run whose
// worktree lives on the runner pod and whose branch-range gitmeta isn't
// recorded until finalize: this server pod has neither, so the listing is
// unavailable — but because the run is still ongoing the reason is
// "building" (not "not_git_repo"/"worktree_gone"), so the studio shows a
// "available when the run finishes" empty state instead of an error.
func TestRunFiles_BuildingWhileRunning(t *testing.T) {
	srv, hs := newTestServer(t)
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.CreateRun(context.Background(), "cloud-live", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.WorkDir = "/nonexistent/runner/pod/worktree"
	r.Worktree = true
	r.Status = store.RunStatusRunning
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	// Default mode (combined) and branch both resolve to "building" while
	// the run is non-terminal.
	for _, mode := range []string{"", "combined", "branch"} {
		resp, err := http.Get(hs.URL + "/api/runs/cloud-live/files?mode=" + mode)
		if err != nil {
			t.Fatal(err)
		}
		var out runFilesResponse
		decodeJSONResp(t, resp, &out)
		if out.Available {
			t.Errorf("mode=%q: Available should be false", mode)
		}
		if out.Reason != "building" {
			t.Errorf("mode=%q: Reason: want building, got %q", mode, out.Reason)
		}
	}
}

func TestRunFileDiff_PathTraversal(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	seedRunWithWorkDir(t, srv, "trav", dir, true)

	resp, err := http.Get(hs.URL + "/api/runs/trav/files/diff?path=../../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRunFileDiff_HappyPath(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedRunWithWorkDir(t, srv, "diff-ok", dir, true)

	resp, err := http.Get(hs.URL + "/api/runs/diff-ok/files/diff?path=a.txt")
	if err != nil {
		t.Fatal(err)
	}
	var out gitlib.DiffPayload
	decodeJSONResp(t, resp, &out)
	if out.Before == nil || *out.Before != "hello\n" {
		t.Errorf("before: %v", out.Before)
	}
	if out.After == nil || *out.After != "changed\n" {
		t.Errorf("after: %v", out.After)
	}
}

// TestRunFiles_ModeCombined verifies the combined view returns the union of
// committed (BaseCommit..HEAD) and uncommitted (`git status`) changes, each
// tagged with the right lifecycle. Fixture: b.txt committed inside the run,
// a.txt modified-but-uncommitted, c.txt untracked.
func TestRunFiles_ModeCombined(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	baseSHA := revParse(t, dir, "HEAD")

	commitInRunWorktree(t, dir, "b.txt", "new\n", "add b")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	seedRunWithBaseline(t, srv, "combined", dir, baseSHA)

	resp, err := http.Get(hs.URL + "/api/runs/combined/files?mode=combined")
	if err != nil {
		t.Fatal(err)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if !out.Available || !out.Live {
		t.Fatalf("Available=%v Live=%v reason=%q", out.Available, out.Live, out.Reason)
	}
	if out.Mode != modeCombined {
		t.Errorf("Mode: want combined, got %q", out.Mode)
	}
	got := map[string]gitlib.FileStatus{}
	for _, f := range out.Files {
		got[f.Path] = f
	}
	if len(out.Files) != 3 {
		t.Errorf("want 3 files (a,b,c), got %d: %+v", len(out.Files), out.Files)
	}
	if got["b.txt"].Lifecycle != lifecycleCommitted {
		t.Errorf("b.txt lifecycle: want %q, got %q", lifecycleCommitted, got["b.txt"].Lifecycle)
	}
	if got["a.txt"].Lifecycle != lifecycleUncommitted {
		t.Errorf("a.txt lifecycle: want %q, got %q", lifecycleUncommitted, got["a.txt"].Lifecycle)
	}
	if got["c.txt"].Lifecycle != lifecycleUncommitted {
		t.Errorf("c.txt lifecycle: want %q, got %q", lifecycleUncommitted, got["c.txt"].Lifecycle)
	}
}

// TestRunFiles_ModeCombined_MixedDedup verifies that a file both committed
// during the run AND re-edited without committing appears exactly once,
// tagged uncommitted (the in-flight state wins on collision).
func TestRunFiles_ModeCombined_MixedDedup(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	baseSHA := revParse(t, dir, "HEAD")

	commitInRunWorktree(t, dir, "b.txt", "v1\n", "add b")
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("v2 pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	seedRunWithBaseline(t, srv, "mixed", dir, baseSHA)

	resp, err := http.Get(hs.URL + "/api/runs/mixed/files?mode=combined")
	if err != nil {
		t.Fatal(err)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if !out.Available {
		t.Fatalf("Available=false reason=%q", out.Reason)
	}
	n := 0
	var entry gitlib.FileStatus
	for _, f := range out.Files {
		if f.Path == "b.txt" {
			n++
			entry = f
		}
	}
	if n != 1 {
		t.Fatalf("b.txt should appear exactly once, got %d: %+v", n, out.Files)
	}
	if entry.Lifecycle != lifecycleUncommitted {
		t.Errorf("mixed b.txt lifecycle: want %q, got %q", lifecycleUncommitted, entry.Lifecycle)
	}
}

// TestRunFiles_ModeCombined_NoBaseline verifies that combined degrades to the
// uncommitted set alone when the run has no BaseCommit (the committed range is
// unknowable) instead of erroring.
func TestRunFiles_ModeCombined_NoBaseline(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedRunWithWorkDir(t, srv, "combined-nobase", dir, true)

	resp, err := http.Get(hs.URL + "/api/runs/combined-nobase/files?mode=combined")
	if err != nil {
		t.Fatal(err)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if !out.Available {
		t.Fatalf("Available=false reason=%q — combined should degrade to uncommitted-only without a baseline", out.Reason)
	}
	if out.Mode != modeCombined {
		t.Errorf("Mode: want combined, got %q", out.Mode)
	}
	if len(out.Files) == 0 {
		t.Fatalf("expected uncommitted files, got none")
	}
	for _, f := range out.Files {
		if f.Lifecycle != lifecycleUncommitted {
			t.Errorf("%s lifecycle: want %q (no baseline → no committed portion), got %q", f.Path, lifecycleUncommitted, f.Lifecycle)
		}
	}
}

// TestRunFiles_ModeCombined_WorktreeGone verifies the combined view reports
// worktree_gone (like uncommitted) once the worktree is torn down, so the UI
// disables the segment and auto-falls-back to branch.
func TestRunFiles_ModeCombined_WorktreeGone(t *testing.T) {
	srv, hs := newTestServer(t)
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.CreateRun(context.Background(), "combined-gone", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.WorkDir = "/nonexistent/host/worktree"
	r.RepoRoot = "/nonexistent/host"
	r.Worktree = true
	r.Status = store.RunStatusFinished
	r.BaseCommit = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	r.FinalCommit = "feedfacefeedfacefeedfacefeedfacefeedface"
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(hs.URL + "/api/runs/combined-gone/files?mode=combined")
	if err != nil {
		t.Fatal(err)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if out.Available {
		t.Errorf("Available should be false")
	}
	if out.Reason != "worktree_gone" {
		t.Errorf("Reason: want worktree_gone, got %q", out.Reason)
	}
}

// The pipeline-board sidebar's inline players key on the artifact-file
// Content-Type: an audio/video extension served as application/octet-stream
// renders as "preview not available" instead of a player.
func TestArtifactFileContentType_MediaTypes(t *testing.T) {
	cases := map[string]string{
		"track.wav":       "audio/wav",
		"theme.MP3":       "audio/mpeg",
		"clip.mp4":        "video/mp4",
		"clip.webm":       "video/webm",
		"cover.jpg":       "image/jpeg",
		"cover.webp":      "image/webp",
		"report.pdf":      "application/pdf",
		"unknown.zzz":     "application/octet-stream",
		"renders/out.mov": "video/quicktime",
	}
	for path, want := range cases {
		if got := artifactFileContentType(path); got != want {
			t.Errorf("artifactFileContentType(%q) = %q, want %q", path, got, want)
		}
	}
}

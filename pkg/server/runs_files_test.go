package server

import (
	"net/http"
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
	r, err := st.CreateRun(runID, "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r.WorkDir = workDir
	r.Worktree = worktree
	if err := st.SaveRun(r); err != nil {
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
	decodeJSON(t, resp, &out)
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
	decodeJSON(t, resp, &out)
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
	decodeJSON(t, resp, &out)
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
	// Live worktree path must mark the response Live=true so the editor
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
	decodeJSON(t, resp, &out)
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
	// even though the storage branch was reachable from the editor's CWD.
	srv, hs := newTestServer(t)

	// Initialise a real repo at the editor's WorkDir (cfg.WorkDir) and put
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
	r, err := st.CreateRun("hist", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.WorkDir = "/nonexistent/host/path/.iterion/worktrees/hist"
	r.RepoRoot = "/nonexistent/host/path"
	r.Worktree = true
	r.BaseCommit = baseSHA
	r.FinalCommit = finalSHA
	if err := st.SaveRun(r); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(hs.URL + "/api/runs/hist/files")
	if err != nil {
		t.Fatal(err)
	}
	var out runFilesResponse
	decodeJSON(t, resp, &out)
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
	decodeJSON(t, resp, &out)
	if out.Before == nil || *out.Before != "hello\n" {
		t.Errorf("before: %v", out.Before)
	}
	if out.After == nil || *out.After != "changed\n" {
		t.Errorf("after: %v", out.After)
	}
}

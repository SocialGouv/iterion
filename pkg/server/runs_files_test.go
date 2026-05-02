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

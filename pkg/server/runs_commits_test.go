package server

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

// commitInRunWorktree creates a real commit on dir and returns its SHA.
// Used by the commit-detail tests to seed a known SHA in the run's range.
func commitInRunWorktree(t *testing.T, dir, relPath, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, relPath), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", relPath}, {"commit", "-q", "-m", msg}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return revParse(t, dir, "HEAD")
}

// seedRunWithBaseline mirrors seedRunWithWorkDir but also records the
// run's BaseCommit so the commit endpoints (which require a baseline)
// have something to range against.
func seedRunWithBaseline(t *testing.T, srv *Server, runID, workDir, base string) {
	t.Helper()
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.CreateRun(context.Background(), runID, "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.WorkDir = workDir
	r.Worktree = true
	r.BaseCommit = base
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func TestRunCommitDetail_HappyPath(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	baseSHA := revParse(t, dir, "HEAD")

	// Two commits inside the "run" — the second one will be the target.
	commitInRunWorktree(t, dir, "b.txt", "hello b\n", "add b")
	target := commitInRunWorktree(t, dir, "c.txt", "hello c\n", "add c")
	seedRunWithBaseline(t, srv, "commit-detail", dir, baseSHA)

	resp, err := http.Get(hs.URL + "/api/runs/commit-detail/commits/" + target)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	var out runCommitDetailResponse
	decodeJSON(t, resp, &out)
	if !out.Available {
		t.Fatalf("Available=false reason=%q", out.Reason)
	}
	if out.SHA != target {
		t.Errorf("SHA: want %q, got %q", target, out.SHA)
	}
	if len(out.Files) != 1 || out.Files[0].Path != "c.txt" || out.Files[0].Status != "A" {
		t.Errorf("files: %+v", out.Files)
	}
}

func TestRunCommitDetail_ShortSHA(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	baseSHA := revParse(t, dir, "HEAD")
	target := commitInRunWorktree(t, dir, "b.txt", "hello b\n", "add b")
	seedRunWithBaseline(t, srv, "short-sha", dir, baseSHA)

	short := target[:8]
	resp, err := http.Get(hs.URL + "/api/runs/short-sha/commits/" + short)
	if err != nil {
		t.Fatal(err)
	}
	var out runCommitDetailResponse
	decodeJSON(t, resp, &out)
	if !out.Available || out.SHA != target {
		t.Errorf("short SHA lookup: %+v", out)
	}
}

func TestRunCommitDetail_OutOfRange(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	// Commit BEFORE the run begins (BaseCommit will be set to this commit).
	// The pre-base commit (initRepo's "init" commit) is then out of the run's
	// range and must be rejected.
	preBase := revParse(t, dir, "HEAD")
	baseSHA := commitInRunWorktree(t, dir, "b.txt", "second\n", "second")
	commitInRunWorktree(t, dir, "c.txt", "third\n", "third")
	seedRunWithBaseline(t, srv, "out-of-range", dir, baseSHA)

	resp, err := http.Get(hs.URL + "/api/runs/out-of-range/commits/" + preBase)
	if err != nil {
		t.Fatal(err)
	}
	var out runCommitDetailResponse
	decodeJSON(t, resp, &out)
	if out.Available {
		t.Fatalf("pre-base commit %q should be unavailable, got %+v", preBase, out)
	}
	if out.Reason != "not_in_range" {
		t.Errorf("reason: want not_in_range, got %q", out.Reason)
	}
}

func TestRunCommitDetail_InvalidSHA(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	seedRunWithBaseline(t, srv, "invalid-sha", dir, revParse(t, dir, "HEAD"))

	resp, err := http.Get(hs.URL + "/api/runs/invalid-sha/commits/not-a-hex")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRunCommitFileDiff_HappyPath(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	baseSHA := revParse(t, dir, "HEAD")

	// Modify a.txt in a commit so we have both before and after content.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-q", "-m", "v2"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	target := revParse(t, dir, "HEAD")
	seedRunWithBaseline(t, srv, "cdiff-ok", dir, baseSHA)

	resp, err := http.Get(hs.URL + "/api/runs/cdiff-ok/commits/" + target + "/diff?path=a.txt")
	if err != nil {
		t.Fatal(err)
	}
	var out gitlib.DiffPayload
	decodeJSON(t, resp, &out)
	if out.Before == nil || *out.Before != "hello\n" {
		t.Errorf("before: %v", out.Before)
	}
	if out.After == nil || *out.After != "v2\n" {
		t.Errorf("after: %v", out.After)
	}
}

func TestRunCommitFileDiff_OutOfRange(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	preBase := revParse(t, dir, "HEAD")
	baseSHA := commitInRunWorktree(t, dir, "b.txt", "second\n", "second")
	seedRunWithBaseline(t, srv, "cdiff-out", dir, baseSHA)

	resp, err := http.Get(hs.URL + "/api/runs/cdiff-out/commits/" + preBase + "/diff?path=a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

// seedCloudRunWithGitMeta creates a run whose WorkDir points at a path that
// does not exist on this pod (the cloud shape: the worktree lived in the
// runner pod and is gone) and records a persisted git-metadata snapshot for
// it. BaseCommit is left empty, exactly as a cloud clone run persists it.
func seedCloudRunWithGitMeta(t *testing.T, srv *Server, runID string, meta *store.RunGitMeta) {
	t.Helper()
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.CreateRun(context.Background(), runID, "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A path that will never exist → dirExists(run.WorkDir) is false, so the
	// live git path is skipped and the persisted fallback must take over.
	r.WorkDir = "/nonexistent/cloud/runner/clone/" + runID
	r.Worktree = false
	// Git meta is recorded at finalize, so a run carrying it is terminal —
	// distinguishes the finalized fallback from a still-"building" cloud run.
	r.Status = store.RunStatusFinished
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if meta != nil {
		if err := st.SaveRunGitMeta(context.Background(), runID, meta); err != nil {
			t.Fatal(err)
		}
	}
}

func sampleGitMeta() *store.RunGitMeta {
	head := "1111111111111111111111111111111111111111"
	c1 := "2222222222222222222222222222222222222222"
	return &store.RunGitMeta{
		BaseCommit: "0000000000000000000000000000000000000000",
		HeadCommit: head,
		Commits: []gitlib.CommitInfo{
			{SHA: c1, Short: "2222222", Subject: "feat: first", Author: "Ada", Email: "ada@x.io", Date: time.Unix(1700000000, 0).UTC()},
			{SHA: head, Short: "1111111", Subject: "feat: second", Author: "Ada", Email: "ada@x.io", Date: time.Unix(1700000100, 0).UTC()},
		},
		Files: []gitlib.FileStatus{
			{Path: "pkg/a.go", Status: "M", Added: 5, Deleted: 1},
			{Path: "pkg/b.go", Status: "A", Added: 20, Deleted: 0},
		},
		CommitFiles: map[string][]gitlib.FileStatus{
			c1:   {{Path: "pkg/a.go", Status: "M", Added: 5, Deleted: 1}},
			head: {{Path: "pkg/b.go", Status: "A", Added: 20, Deleted: 0}},
		},
	}
}

func TestRunCommits_PersistedFallback(t *testing.T) {
	srv, hs := newTestServer(t)
	seedCloudRunWithGitMeta(t, srv, "cloud-commits", sampleGitMeta())

	resp, err := http.Get(hs.URL + "/api/runs/cloud-commits/commits")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out runCommitsResponse
	decodeJSONResp(t, resp, &out)
	if !out.Available {
		t.Fatalf("Available=false reason=%q, want persisted commits", out.Reason)
	}
	if out.Count != 2 || len(out.Commits) != 2 {
		t.Fatalf("Count=%d commits=%+v, want 2", out.Count, out.Commits)
	}
	if out.Commits[0].Subject != "feat: first" || out.Commits[1].Subject != "feat: second" {
		t.Errorf("commit subjects = %q, %q", out.Commits[0].Subject, out.Commits[1].Subject)
	}
	if out.HeadCommit != "1111111111111111111111111111111111111111" {
		t.Errorf("HeadCommit = %q", out.HeadCommit)
	}
	if out.DefaultSquashMessage == "" {
		t.Error("DefaultSquashMessage empty, want a summary derived from commits")
	}
}

func TestRunFiles_PersistedFallback(t *testing.T) {
	srv, hs := newTestServer(t)
	seedCloudRunWithGitMeta(t, srv, "cloud-files", sampleGitMeta())

	// Default mode (no ?mode=) must resolve to the branch view from meta.
	resp, err := http.Get(hs.URL + "/api/runs/cloud-files/files")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if !out.Available {
		t.Fatalf("Available=false reason=%q, want persisted files", out.Reason)
	}
	if out.Live {
		t.Error("Live=true, want false for a persisted (historical) snapshot")
	}
	if out.Mode != modeBranch {
		t.Errorf("Mode = %q, want branch", out.Mode)
	}
	if len(out.Files) != 2 {
		t.Fatalf("files = %+v, want 2", out.Files)
	}
	paths := map[string]string{}
	for _, f := range out.Files {
		paths[f.Path] = f.Status
	}
	if paths["pkg/a.go"] != "M" || paths["pkg/b.go"] != "A" {
		t.Errorf("files = %+v, want a.go M + b.go A", out.Files)
	}
}

func TestRunFiles_PersistedFallback_CombinedTagsCommitted(t *testing.T) {
	srv, hs := newTestServer(t)
	seedCloudRunWithGitMeta(t, srv, "cloud-combined", sampleGitMeta())

	resp, err := http.Get(hs.URL + "/api/runs/cloud-combined/files?mode=combined")
	if err != nil {
		t.Fatal(err)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if !out.Available || out.Mode != modeCombined {
		t.Fatalf("available=%v mode=%q, want available combined", out.Available, out.Mode)
	}
	for _, f := range out.Files {
		if f.Lifecycle != lifecycleCommitted {
			t.Errorf("file %q lifecycle = %q, want committed", f.Path, f.Lifecycle)
		}
	}
}

func TestRunFiles_PersistedFallback_UncommittedStillGone(t *testing.T) {
	srv, hs := newTestServer(t)
	seedCloudRunWithGitMeta(t, srv, "cloud-uncommitted", sampleGitMeta())

	// The persisted snapshot cannot reconstruct uncommitted state; a strict
	// uncommitted request must still report worktree_gone.
	resp, err := http.Get(hs.URL + "/api/runs/cloud-uncommitted/files?mode=uncommitted")
	if err != nil {
		t.Fatal(err)
	}
	var out runFilesResponse
	decodeJSONResp(t, resp, &out)
	if out.Available || out.Reason != "worktree_gone" {
		t.Errorf("available=%v reason=%q, want unavailable worktree_gone", out.Available, out.Reason)
	}
}

func TestRunCommitDetail_PersistedFallback(t *testing.T) {
	srv, hs := newTestServer(t)
	seedCloudRunWithGitMeta(t, srv, "cloud-detail", sampleGitMeta())

	target := "2222222222222222222222222222222222222222"
	resp, err := http.Get(hs.URL + "/api/runs/cloud-detail/commits/" + target)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out runCommitDetailResponse
	decodeJSONResp(t, resp, &out)
	if !out.Available {
		t.Fatalf("Available=false reason=%q", out.Reason)
	}
	if out.SHA != target || out.Subject != "feat: first" {
		t.Errorf("sha/subject = %q/%q", out.SHA, out.Subject)
	}
	// First commit's parent is the recorded BaseCommit.
	if out.Parent != "0000000000000000000000000000000000000000" {
		t.Errorf("parent = %q, want BaseCommit", out.Parent)
	}
	if len(out.Files) != 1 || out.Files[0].Path != "pkg/a.go" {
		t.Errorf("files = %+v, want pkg/a.go", out.Files)
	}
}

// A cloud run that made no commits records an empty snapshot: the panels
// must show the "no commits" state (available, count 0), not an error.
func TestRunCommits_PersistedFallback_NoCommits(t *testing.T) {
	srv, hs := newTestServer(t)
	seedCloudRunWithGitMeta(t, srv, "cloud-nocommits", &store.RunGitMeta{
		BaseCommit: "abc",
		HeadCommit: "abc",
		Commits:    []gitlib.CommitInfo{},
		Files:      []gitlib.FileStatus{},
	})

	resp, err := http.Get(hs.URL + "/api/runs/cloud-nocommits/commits")
	if err != nil {
		t.Fatal(err)
	}
	var out runCommitsResponse
	decodeJSONResp(t, resp, &out)
	if !out.Available || out.Count != 0 {
		t.Errorf("available=%v count=%d, want available with 0 commits", out.Available, out.Count)
	}
}

// With no persisted snapshot and no live worktree, the endpoints fall
// through to their neutral empty states (unchanged behaviour).
func TestRunCommitsFiles_NoMetaNoWorktree(t *testing.T) {
	srv, hs := newTestServer(t)
	seedCloudRunWithGitMeta(t, srv, "cloud-nometa", nil)

	cResp, err := http.Get(hs.URL + "/api/runs/cloud-nometa/commits")
	if err != nil {
		t.Fatal(err)
	}
	var c runCommitsResponse
	decodeJSONResp(t, cResp, &c)
	if c.Available {
		t.Error("commits Available=true without any source")
	}

	fResp, err := http.Get(hs.URL + "/api/runs/cloud-nometa/files")
	if err != nil {
		t.Fatal(err)
	}
	var f runFilesResponse
	decodeJSONResp(t, fResp, &f)
	if f.Available {
		t.Error("files Available=true without any source")
	}
}

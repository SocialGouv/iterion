package store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2024-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2024-01-01T00:00:00Z")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestBuildRunGitMeta(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "user.email", "base@example.com")
	gitRun(t, dir, "config", "user.name", "Baseliner")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "a.txt")
	gitRun(t, dir, "commit", "-q", "-m", "base")

	baseCmd := exec.Command("git", "rev-parse", "HEAD")
	baseCmd.Dir = dir
	baseOut, err := baseCmd.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	base := string(baseOut[:len(baseOut)-1])

	// No commits yet beyond base → empty snapshot.
	meta, err := BuildRunGitMeta(dir, base)
	if err != nil {
		t.Fatalf("BuildRunGitMeta (no commits): %v", err)
	}
	if len(meta.Commits) != 0 || len(meta.Files) != 0 {
		t.Errorf("no-commit meta = %+v, want empty commits/files", meta)
	}
	if meta.HeadCommit != base {
		t.Errorf("head = %q, want base %q", meta.HeadCommit, base)
	}

	// Two run commits on top of base.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "feat: add b, edit a")
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "feat: add c")

	meta, err = BuildRunGitMeta(dir, base)
	if err != nil {
		t.Fatalf("BuildRunGitMeta: %v", err)
	}
	if len(meta.Commits) != 2 {
		t.Fatalf("commits = %d, want 2 (%+v)", len(meta.Commits), meta.Commits)
	}
	// git log --reverse: oldest first.
	if meta.Commits[0].Subject != "feat: add b, edit a" || meta.Commits[1].Subject != "feat: add c" {
		t.Errorf("commit order/subjects = %q, %q", meta.Commits[0].Subject, meta.Commits[1].Subject)
	}
	// Modified-files vs base: a.txt (M), b.txt (A), c.txt (A).
	paths := map[string]string{}
	for _, f := range meta.Files {
		paths[f.Path] = f.Status
	}
	if paths["a.txt"] != "M" || paths["b.txt"] != "A" || paths["c.txt"] != "A" {
		t.Errorf("files = %+v, want a.txt M, b.txt A, c.txt A", meta.Files)
	}
	// Per-commit files recorded.
	if len(meta.CommitFiles) != 2 {
		t.Errorf("commit_files entries = %d, want 2", len(meta.CommitFiles))
	}
}

func TestFilesystemRunGitMeta_SaveLoadRoundTrip(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, "run-gitmeta")

	// AsRunGitMetaStore must resolve the filesystem store.
	gs := AsRunGitMetaStore(s)
	if gs == nil {
		t.Fatal("AsRunGitMetaStore returned nil for FilesystemRunStore")
	}

	// A run with no recorded metadata reads back nil, nil.
	if got, err := gs.LoadRunGitMeta(ctx, "run-gitmeta"); err != nil || got != nil {
		t.Fatalf("LoadRunGitMeta before save = (%v, %v), want (nil, nil)", got, err)
	}

	meta := &RunGitMeta{
		BaseCommit: "aaaa000000000000000000000000000000000000",
		HeadCommit: "bbbb000000000000000000000000000000000000",
		Commits: []gitlib.CommitInfo{
			{SHA: "bbbb000000000000000000000000000000000000", Short: "bbbb000", Subject: "feat: thing", Author: "Ada", Email: "ada@example.com", Date: time.Unix(1700000000, 0).UTC()},
		},
		Files: []gitlib.FileStatus{
			{Path: "pkg/foo.go", Status: "M", Added: 10, Deleted: 2},
		},
		CommitFiles: map[string][]gitlib.FileStatus{
			"bbbb000000000000000000000000000000000000": {{Path: "pkg/foo.go", Status: "M", Added: 10, Deleted: 2}},
		},
	}
	if err := gs.SaveRunGitMeta(ctx, "run-gitmeta", meta); err != nil {
		t.Fatalf("SaveRunGitMeta: %v", err)
	}
	if meta.UpdatedAt.IsZero() {
		t.Error("SaveRunGitMeta did not stamp UpdatedAt")
	}

	got, err := gs.LoadRunGitMeta(ctx, "run-gitmeta")
	if err != nil {
		t.Fatalf("LoadRunGitMeta: %v", err)
	}
	if got == nil {
		t.Fatal("LoadRunGitMeta returned nil after save")
	}
	if got.BaseCommit != meta.BaseCommit || got.HeadCommit != meta.HeadCommit {
		t.Errorf("base/head = %q/%q, want %q/%q", got.BaseCommit, got.HeadCommit, meta.BaseCommit, meta.HeadCommit)
	}
	if len(got.Commits) != 1 || got.Commits[0].Subject != "feat: thing" {
		t.Errorf("commits = %+v, want one 'feat: thing'", got.Commits)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "pkg/foo.go" || got.Files[0].Added != 10 {
		t.Errorf("files = %+v, want pkg/foo.go +10", got.Files)
	}
	if cf := got.CommitFiles["bbbb000000000000000000000000000000000000"]; len(cf) != 1 {
		t.Errorf("commit_files = %+v, want one entry", got.CommitFiles)
	}
}

func TestFilesystemRunGitMeta_Overwrite(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, "run-ow")
	gs := AsRunGitMetaStore(s)

	if err := gs.SaveRunGitMeta(ctx, "run-ow", &RunGitMeta{HeadCommit: "one"}); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if err := gs.SaveRunGitMeta(ctx, "run-ow", &RunGitMeta{HeadCommit: "two", Commits: []gitlib.CommitInfo{{SHA: "x"}}}); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	got, err := gs.LoadRunGitMeta(ctx, "run-ow")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.HeadCommit != "two" || len(got.Commits) != 1 {
		t.Errorf("overwrite = %+v, want head=two + 1 commit (latest snapshot wins)", got)
	}
}

func TestFilesystemRunGitMeta_RejectsUnsafeRunID(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	if err := s.SaveRunGitMeta(ctx, "../escape", &RunGitMeta{}); err == nil {
		t.Error("SaveRunGitMeta accepted a path-traversal run ID")
	}
}

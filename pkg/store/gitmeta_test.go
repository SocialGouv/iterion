package store

import (
	"context"
	"testing"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

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

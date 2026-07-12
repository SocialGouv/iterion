package store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// setupDiffRepo builds a repo with a base commit plus two run commits and
// returns (dir, base). Mirrors gitmeta_test's helper style.
func setupDiffRepo(t *testing.T) (string, string) {
	t.Helper()
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
	baseOut, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	base := strings.TrimSpace(string(baseOut))

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "feat: edit a, add b")
	return dir, base
}

func TestPopulateRunDiffs_Inline(t *testing.T) {
	dir, base := setupDiffRepo(t)
	meta, err := BuildRunGitMeta(dir, base)
	if err != nil {
		t.Fatalf("BuildRunGitMeta: %v", err)
	}

	// No sink → everything small stays inline.
	PopulateRunDiffs(context.Background(), "run-x", dir, meta, nil)

	if meta.DiffsTruncated {
		t.Errorf("small run marked DiffsTruncated")
	}
	fd := meta.FileDiffs["a.txt"]
	if fd == nil {
		t.Fatalf("no FileDiff for a.txt; got %+v", meta.FileDiffs)
	}
	if fd.Before == nil || *fd.Before != "one\n" {
		t.Errorf("a.txt before = %v, want 'one\\n'", fd.Before)
	}
	if fd.After == nil || *fd.After != "two\n" {
		t.Errorf("a.txt after = %v, want 'two\\n'", fd.After)
	}
	// b.txt is an add: before nil, after content.
	if bfd := meta.FileDiffs["b.txt"]; bfd == nil || bfd.Before != nil || bfd.After == nil || *bfd.After != "new file\n" {
		t.Errorf("b.txt diff = %+v, want add with after 'new file\\n'", bfd)
	}

	// Per-commit diffs populated for the run commit.
	if len(meta.CommitFileDiffs) != 1 {
		t.Fatalf("commit_file_diffs = %d, want 1", len(meta.CommitFileDiffs))
	}
	for _, perPath := range meta.CommitFileDiffs {
		if perPath["a.txt"] == nil || perPath["b.txt"] == nil {
			t.Errorf("commit diffs missing a.txt/b.txt: %+v", perPath)
		}
	}

	// Round-trips as a DiffPayload (no blob store needed for inline).
	p := ResolveRunFileDiff(context.Background(), nil, "run-x", fd)
	if p.After == nil || *p.After != "two\n" {
		t.Errorf("ResolveRunFileDiff after = %v, want 'two\\n'", p.After)
	}
}

func TestPopulateRunDiffs_BudgetTruncates(t *testing.T) {
	dir, base := setupDiffRepo(t)
	meta, err := BuildRunGitMeta(dir, base)
	if err != nil {
		t.Fatalf("BuildRunGitMeta: %v", err)
	}
	// A zero-remaining budget forces truncation on the first content-bearing
	// file (drive it directly through diffBudget.store).
	b := &diffBudget{remaining: 0}
	p, _ := gitlib.DiffBetween(dir, base, meta.HeadCommit, "a.txt")
	fd := b.store(context.Background(), nil, "run-x", diffBlobRef("range", "a.txt"), p, meta)
	if !fd.Truncated || fd.Before != nil || fd.After != nil {
		t.Errorf("over-budget file = %+v, want Truncated with no content", fd)
	}
	if !meta.DiffsTruncated {
		t.Errorf("meta.DiffsTruncated not set after truncation")
	}
	// Truncated resolves to an Oversized placeholder, never a nil crash.
	rp := ResolveRunFileDiff(context.Background(), nil, "run-x", fd)
	if !rp.Oversized {
		t.Errorf("resolved truncated diff = %+v, want Oversized", rp)
	}
}

func TestFilesystemRunDiffBlob_RoundTrip(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, "run-blob")

	bs := AsRunDiffBlobStore(s)
	if bs == nil {
		t.Fatal("AsRunDiffBlobStore returned nil for FilesystemRunStore")
	}
	ref := diffBlobRef("range", "pkg/big.go")
	body := []byte(`{"path":"pkg/big.go","after":"x"}`)
	if err := bs.PutRunDiffBlob(ctx, "run-blob", ref, body); err != nil {
		t.Fatalf("PutRunDiffBlob: %v", err)
	}
	got, err := bs.GetRunDiffBlob(ctx, "run-blob", ref)
	if err != nil {
		t.Fatalf("GetRunDiffBlob: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("blob round-trip = %q, want %q", got, body)
	}

	// A path-traversal run ID / ref is rejected.
	if err := s.PutRunDiffBlob(ctx, "../escape", ref, body); err == nil {
		t.Error("PutRunDiffBlob accepted a path-traversal run ID")
	}

	// DeleteRun reclaims the blob directory (whole run dir removed).
	if err := s.DeleteRun(ctx, "run-blob"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if _, err := bs.GetRunDiffBlob(ctx, "run-blob", ref); err == nil {
		t.Error("GetRunDiffBlob succeeded after DeleteRun")
	}
}

func TestFilesystemRunGitMeta_DiffsRoundTrip(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, "run-fd")
	gs := AsRunGitMetaStore(s)

	before, after := "old\n", "new\n"
	meta := &RunGitMeta{
		BaseCommit: "aaaa",
		HeadCommit: "bbbb",
		FileDiffs: map[string]*RunFileDiff{
			"a.txt": {Path: "a.txt", Before: &before, After: &after},
		},
		CommitFileDiffs: map[string]map[string]*RunFileDiff{
			"bbbb": {"a.txt": {Path: "a.txt", After: &after}},
		},
		DiffsTruncated: true,
	}
	if err := gs.SaveRunGitMeta(ctx, "run-fd", meta); err != nil {
		t.Fatalf("SaveRunGitMeta: %v", err)
	}
	got, err := gs.LoadRunGitMeta(ctx, "run-fd")
	if err != nil {
		t.Fatalf("LoadRunGitMeta: %v", err)
	}
	if got.FileDiffs["a.txt"] == nil || got.FileDiffs["a.txt"].After == nil || *got.FileDiffs["a.txt"].After != "new\n" {
		t.Errorf("file_diffs round-trip = %+v", got.FileDiffs)
	}
	if got.CommitFileDiffs["bbbb"]["a.txt"] == nil {
		t.Errorf("commit_file_diffs round-trip = %+v", got.CommitFileDiffs)
	}
	if !got.DiffsTruncated {
		t.Errorf("DiffsTruncated not persisted")
	}
}

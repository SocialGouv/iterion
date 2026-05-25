package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolveSHA reads HEAD's SHA in dir for tests that need to refer back to
// specific commits without parsing the test's `git log`.
func resolveSHA(t *testing.T, dir, ref string) string {
	t.Helper()
	sha, err := RevParseHead(dir)
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return sha
}

// commit writes content to relPath and creates a commit. Returns the new SHA.
func commit(t *testing.T, dir, relPath, content, msg string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, relPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, relPath), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "add", relPath)
	mustRun(t, dir, "commit", "-q", "-m", msg)
	return resolveSHA(t, dir, "HEAD")
}

func TestCommitParent(t *testing.T) {
	dir := gitRepo(t)
	root := resolveSHA(t, dir, "HEAD")
	parent, err := CommitParent(dir, root)
	if err != nil {
		t.Fatalf("CommitParent(root): %v", err)
	}
	if parent != "" {
		t.Errorf("root commit parent: want empty, got %q", parent)
	}

	second := commit(t, dir, "b.txt", "second\n", "add b")
	got, err := CommitParent(dir, second)
	if err != nil {
		t.Fatalf("CommitParent(second): %v", err)
	}
	if got != root {
		t.Errorf("second parent: want %q, got %q", root, got)
	}
}

func TestCommitParentUnknown(t *testing.T) {
	dir := gitRepo(t)
	_, err := CommitParent(dir, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if err == nil {
		t.Fatal("want error for unknown sha, got nil")
	}
}

func TestShowCommitMidHistory(t *testing.T) {
	dir := gitRepo(t)
	sha := commit(t, dir, "b.txt", "second\n", "add b")
	files, err := ShowCommit(dir, sha)
	if err != nil {
		t.Fatalf("ShowCommit: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %+v", files)
	}
	if files[0].Path != "b.txt" || files[0].Status != "A" {
		t.Errorf("entry: %+v", files[0])
	}
	if files[0].Added != 1 {
		t.Errorf("added: want 1, got %d", files[0].Added)
	}
}

func TestShowCommitRoot(t *testing.T) {
	dir := gitRepo(t)
	root := resolveSHA(t, dir, "HEAD")
	files, err := ShowCommit(dir, root)
	if err != nil {
		t.Fatalf("ShowCommit(root): %v", err)
	}
	if len(files) != 1 || files[0].Path != "a.txt" || files[0].Status != "A" {
		t.Fatalf("root commit files: %+v", files)
	}
}

func TestDiffOfCommitMidHistory(t *testing.T) {
	dir := gitRepo(t)
	// Modify a.txt across one commit so we can verify before/after.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "add", "a.txt")
	mustRun(t, dir, "commit", "-q", "-m", "edit a")
	sha := resolveSHA(t, dir, "HEAD")
	d, err := DiffOfCommit(dir, sha, "a.txt")
	if err != nil {
		t.Fatalf("DiffOfCommit: %v", err)
	}
	if d.Before == nil || *d.Before != "hello\n" {
		t.Errorf("before: %v", d.Before)
	}
	if d.After == nil || *d.After != "changed\n" {
		t.Errorf("after: %v", d.After)
	}
}

func TestDiffOfCommitRoot(t *testing.T) {
	dir := gitRepo(t)
	root := resolveSHA(t, dir, "HEAD")
	d, err := DiffOfCommit(dir, root, "a.txt")
	if err != nil {
		t.Fatalf("DiffOfCommit(root): %v", err)
	}
	if d.Before != nil {
		t.Errorf("before should be nil for root commit, got %q", *d.Before)
	}
	if d.After == nil || *d.After != "hello\n" {
		t.Errorf("after: %v", d.After)
	}
}

func TestDiffOfCommitNotGitRepo(t *testing.T) {
	dir := t.TempDir()
	_, err := DiffOfCommit(dir, "deadbeef", "a.txt")
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("want ErrNotGitRepo, got %v", err)
	}
}

// TestFindMainRepoRoot_LinkedWorktree locks the fix that lets
// dispatcher-spawned bot runs (which execute in a linked worktree at
// `<repo>/.iterion/dispatcher/workspaces/<id>`) resolve back to the
// operator's main checkout for project-rooted memory keying. Without
// this, `${PROJECT_MEMORY_DIR}/<scope>` keyed off the encoded worktree
// path and shared project-rooted scopes (session-continuity memory,
// historically the findings inbox before it moved to the board) broke
// silently — bot runs landed memory at
// `/home/jo/.iterion/projects/-home-jo-lab-ai-iterion-.iterion-dispatcher-workspaces-native_<id>/memory/<scope>/`
// rather than the project-rooted dir Nexie reads.
func TestFindMainRepoRoot_LinkedWorktree(t *testing.T) {
	main := gitRepo(t)
	// Create a linked worktree under the main repo. We use the same
	// `<repo>/.iterion/dispatcher/workspaces/<name>` layout the dispatcher
	// uses so the test mirrors the production path shape.
	wt := filepath.Join(main, ".iterion", "dispatcher", "workspaces", "native_test")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun(t, main, "worktree", "add", "--detach", wt, "HEAD")

	// FindRepoRoot (legacy) sees `.git` (pointer file) in the worktree
	// and returns the worktree itself — that's the pre-fix behaviour
	// we're documenting + asserting hasn't regressed.
	if got := FindRepoRoot(wt); got != wt {
		// Resolve symlinks for the comparison (TempDir on macOS goes
		// through /var → /private/var).
		gotAbs, _ := filepath.EvalSymlinks(got)
		wtAbs, _ := filepath.EvalSymlinks(wt)
		if gotAbs != wtAbs {
			t.Fatalf("FindRepoRoot on worktree: got %q, want worktree path %q", got, wt)
		}
	}

	// FindMainRepoRoot follows the worktree's `.git` pointer file back
	// to the main checkout.
	got := FindMainRepoRoot(wt)
	gotAbs, _ := filepath.EvalSymlinks(got)
	mainAbs, _ := filepath.EvalSymlinks(main)
	if gotAbs != mainAbs {
		t.Fatalf("FindMainRepoRoot on worktree: got %q, want main %q", got, main)
	}

	// FindMainRepoRoot from the main repo itself stays at the main repo.
	got = FindMainRepoRoot(main)
	gotAbs, _ = filepath.EvalSymlinks(got)
	if gotAbs != mainAbs {
		t.Fatalf("FindMainRepoRoot on main: got %q, want %q", got, main)
	}

	// FindMainRepoRoot starting from a subdir of the worktree also
	// resolves to main (walks up to .git pointer first, then follows).
	sub := filepath.Join(wt, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got = FindMainRepoRoot(sub)
	gotAbs, _ = filepath.EvalSymlinks(got)
	if gotAbs != mainAbs {
		t.Fatalf("FindMainRepoRoot on worktree subdir: got %q, want %q", got, main)
	}
}

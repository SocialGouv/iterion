package context

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTestRepo creates a temporary git repo with some commits.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	run("init", "-b", "main")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")

	// Create first commit.
	os.WriteFile(filepath.Join(dir, "file1.txt"), []byte("hello"), 0o644)
	run("add", "file1.txt")
	run("commit", "-m", "first commit")

	// Create second commit.
	os.WriteFile(filepath.Join(dir, "file2.txt"), []byte("world"), 0o644)
	run("add", "file2.txt")
	run("commit", "-m", "second commit")

	return dir
}

func TestCollectWorktreeInfo(t *testing.T) {
	dir := initTestRepo(t)

	info := CollectWorktreeInfo(dir)
	if info == nil {
		t.Fatal("expected non-nil info for git repo")
	}

	if info.Branch != "main" {
		t.Errorf("Branch = %q, want %q", info.Branch, "main")
	}

	if info.IsDetached {
		t.Error("should not be detached")
	}

	if info.SHA == "" {
		t.Error("SHA should not be empty")
	}

	if len(info.RecentCommits) != 2 {
		t.Errorf("RecentCommits: got %d, want 2", len(info.RecentCommits))
	}
}

func TestCollectWorktreeInfoNotGitRepo(t *testing.T) {
	dir := t.TempDir()
	info := CollectWorktreeInfo(dir)
	if info != nil {
		t.Error("expected nil for non-git directory")
	}
}

func TestRecentCommits(t *testing.T) {
	dir := initTestRepo(t)

	commits := RecentCommits(dir, 2)
	if len(commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(commits))
	}

	// Most recent first.
	if commits[0].Subject != "second commit" {
		t.Errorf("first commit subject = %q, want %q", commits[0].Subject, "second commit")
	}
	if commits[1].Subject != "first commit" {
		t.Errorf("second commit subject = %q, want %q", commits[1].Subject, "first commit")
	}

	// Hash should be non-empty.
	for _, c := range commits {
		if c.Hash == "" {
			t.Error("commit hash should not be empty")
		}
	}
}

func TestRecentCommitsLimit(t *testing.T) {
	dir := initTestRepo(t)

	commits := RecentCommits(dir, 1)
	if len(commits) != 1 {
		t.Fatalf("expected 1 commit with limit=1, got %d", len(commits))
	}
}

func TestGitStatus(t *testing.T) {
	dir := initTestRepo(t)
	status := GitStatus(dir)
	if status == "" {
		t.Error("expected non-empty status for git repo")
	}

	// Should contain branch info.
	if !contains(status, "Current branch: main") {
		t.Errorf("status should contain branch info:\n%s", status)
	}
}

func TestGitStatusNonGitDir(t *testing.T) {
	status := GitStatus(t.TempDir())
	if status != "" {
		t.Errorf("expected empty status for non-git dir, got %q", status)
	}
}

func TestCollectWorktreeInfoModifiedFiles(t *testing.T) {
	dir := initTestRepo(t)

	// Create untracked file.
	os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new"), 0o644)

	// Modify tracked file.
	os.WriteFile(filepath.Join(dir, "file1.txt"), []byte("modified"), 0o644)

	info := CollectWorktreeInfo(dir)
	if info == nil {
		t.Fatal("expected non-nil info")
	}

	if info.Untracked != 1 {
		t.Errorf("Untracked = %d, want 1", info.Untracked)
	}
	if info.Modified != 1 {
		t.Errorf("Modified = %d, want 1", info.Modified)
	}
}

func TestCollectWorktreeInfoStagedFiles(t *testing.T) {
	dir := initTestRepo(t)

	// Stage a new file.
	os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged"), 0o644)
	cmd := exec.Command("git", "add", "staged.txt")
	cmd.Dir = dir
	cmd.Run()

	info := CollectWorktreeInfo(dir)
	if info == nil {
		t.Fatal("expected non-nil info")
	}

	if len(info.StagedFiles) != 1 || info.StagedFiles[0] != "staged.txt" {
		t.Errorf("StagedFiles = %v, want [staged.txt]", info.StagedFiles)
	}
}

func TestGitWorktreeInfoStatusPorcelainCached(t *testing.T) {
	dir := initTestRepo(t)
	// Create an untracked file so porcelain output is non-empty.
	os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new"), 0o644)

	info := CollectWorktreeInfo(dir)
	if info == nil {
		t.Fatal("expected non-nil info")
	}
	if info.StatusPorcelain == "" {
		t.Error("StatusPorcelain should be non-empty when there are changes")
	}
	if !strings.Contains(info.StatusPorcelain, "untracked.txt") {
		t.Errorf("StatusPorcelain should contain untracked file, got: %s", info.StatusPorcelain)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

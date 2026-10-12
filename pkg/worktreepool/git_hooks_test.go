package worktreepool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// The pool inspects run worktrees, whose repository config a run can write:
// the program `core.fsmonitor` names runs whenever git refreshes the index,
// a read-only `git status` included. The pool's git runs none.
func TestPoolGitRunsNoConfiguredMonitor(t *testing.T) {
	repo := t.TempDir()
	gittest.InitRepo(t, repo)
	marker := filepath.Join(t.TempDir(), "ran")
	monitor := filepath.Join(t.TempDir(), "monitor.sh")
	if err := os.WriteFile(monitor, []byte("#!/bin/sh\n: > '"+marker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, repo, "config", "core.fsmonitor", monitor)
	if _, err := gitOut(repo, "status", "--porcelain"); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("inspecting a run's worktree ran the program its repository's config names")
	}
}

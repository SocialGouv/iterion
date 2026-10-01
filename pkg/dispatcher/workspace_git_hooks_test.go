package dispatcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// A dispatched workspace is a run's tree: its hooks directory and its config
// are the run's to write. Asking whether it is dirty refreshes the index, and
// git then runs the program `core.fsmonitor` names and the post-index-change
// hook. The dispatcher's git runs neither.
func TestWorkspaceIsDirtyRunsNoRepositoryHook(t *testing.T) {
	repo := t.TempDir()
	gittest.InitRepo(t, repo)
	marker := filepath.Join(t.TempDir(), "ran")
	monitor := filepath.Join(t.TempDir(), "monitor.sh")
	if err := os.WriteFile(monitor, []byte("#!/bin/sh\n: > '"+marker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, repo, "config", "core.fsmonitor", monitor)
	hooks := filepath.Join(repo, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "post-index-change"), []byte("#!/bin/sh\n: > '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err := workspaceIsDirty(repo)
	if err != nil || !dirty {
		t.Fatalf("workspaceIsDirty = %v, %v; want a dirty workspace", dirty, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("inspecting a run's workspace ran a program its repository names")
	}
}

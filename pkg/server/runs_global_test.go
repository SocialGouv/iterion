package server

import (
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestGlobalActiveStatusIncludesOperatorPause(t *testing.T) {
	if !isGlobalDiscoverableStatus(store.RunStatusPausedOperator) {
		t.Fatal("operator-paused run should remain discoverable across stores")
	}
}

func TestOnlyRunningGlobalRunNeedsHeartbeat(t *testing.T) {
	if !globalRunNeedsHeartbeat(store.RunStatusRunning) {
		t.Fatal("running run should use its event heartbeat")
	}
	for _, status := range []store.RunStatus{
		store.RunStatusPausedWaitingHuman,
		store.RunStatusPausedOperator,
		store.RunStatusQueued,
	} {
		if globalRunNeedsHeartbeat(status) {
			t.Fatalf("%s must remain discoverable while intentionally quiet", status)
		}
	}
}

func TestWorkspaceDirForGlobalRunPrefersPersistedWorkDir(t *testing.T) {
	storePath := filepath.Join(
		"/home/user/.iterion/projects",
		"-home-user-Workspace-game-town-iterion-bots-town-planner",
	)
	rec := runJSONShape{WorkDir: "/home/user/Workspace/game/town"}

	if got, want := workspaceDirForGlobalRun(storePath, rec), rec.WorkDir; got != want {
		t.Fatalf("workspace = %q, want persisted work_dir %q", got, want)
	}
}

func TestWorkspaceDirForGlobalRunPrefersRepoRootForWorktree(t *testing.T) {
	rec := runJSONShape{
		WorkDir:  "/home/user/.iterion/worktrees/run-1",
		RepoRoot: "/home/user/Workspace/game/town",
	}

	if got, want := workspaceDirForGlobalRun("/unused", rec), rec.RepoRoot; got != want {
		t.Fatalf("workspace = %q, want repo_root %q", got, want)
	}
}

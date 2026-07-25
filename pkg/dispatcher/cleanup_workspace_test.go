package dispatcher

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// newCleanupTestDispatcher builds a minimal, un-started dispatcher with a
// given workspace-persist policy. The actor loop is NOT running, so tests
// drive finishRun / cleanupWorkspace directly on the calling goroutine.
func newCleanupTestDispatcher(t *testing.T, persist WorkspacePersistPolicy, wsRoot string) (*Dispatcher, *Workspaces) {
	t.Helper()
	cfg := &Config{
		Name:      "test",
		Workflow:  t.TempDir() + "/fake.bot",
		Tracker:   TrackerConfig{Kind: "fake"},
		Polling:   PollingConfig{IntervalMS: 50},
		Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000, RunningState: "in_progress"},
		Workspace: WorkspaceConfig{Root: wsRoot, Persist: persist},
		Stall:     StallConfig{TimeoutMS: 0},
	}
	cfg.applyDefaults()
	ws, err := NewWorkspaces(wsRoot)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	c, err := New(Options{
		Config:     cfg,
		Tracker:    newFakeTracker(),
		Runner:     &StubRunner{},
		Workspaces: ws,
		Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, ws
}

// TestCleanupWorkspace_RunsBeforeRemoveBeforeDeletingDir is the core
// regression guard for the "before_remove is never invoked" bug. Under a
// cleanup policy, cleanupWorkspace must run the before_remove hook AND it
// must run while the workspace still exists (so the default `git worktree
// remove` can deregister it), THEN delete the directory.
func TestCleanupWorkspace_RunsBeforeRemoveBeforeDeletingDir(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	c, ws := newCleanupTestDispatcher(t, WorkspacePersistCleanupOnDone, wsRoot)

	issueID := "fake:cleanup-1"
	wsPath, _, err := ws.Create(issueID)
	if err != nil {
		t.Fatalf("ws.Create: %v", err)
	}

	// before_remove records — into a sentinel OUTSIDE the workspace —
	// whether $ITERION_WORKSPACE still existed when the hook ran. The file
	// is written only if the hook ran AND the directory was still present;
	// its absence means the hook never fired (the original bug).
	sentinel := filepath.Join(dir, "before_remove_saw")
	hook := &Hook{Script: fmt.Sprintf(
		`if [ -d "$ITERION_WORKSPACE" ]; then printf '%%s' "$ITERION_WORKSPACE" > %q; fi`, sentinel)}

	entry := &runningEntry{
		IssueID:       issueID,
		Identifier:    "fake#cleanup-1",
		RunID:         "run-cleanup-1",
		WorkflowState: "in_progress",
		WorkspacePath: wsPath,
	}
	env := c.dispatchEnv(entry, DispatchSpec{RunID: entry.RunID, WorkspacePath: wsPath})

	c.cleanupWorkspace(entry, hook, env)

	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("before_remove sentinel missing — hook never ran: %v", err)
	}
	if string(got) != wsPath {
		t.Fatalf("before_remove saw %q, want %q — hook must run while the workspace still exists", got, wsPath)
	}
	if _, err := os.Stat(wsPath); !os.IsNotExist(err) {
		t.Fatalf("workspace %q not removed after cleanup (stat err=%v)", wsPath, err)
	}
}

// TestCleanupWorkspace_RemovesEvenWhenBeforeRemoveFails asserts the
// best-effort contract: a failing before_remove is logged but the directory
// is still removed, so a bad hook never strands the workspace on disk.
func TestCleanupWorkspace_RemovesEvenWhenBeforeRemoveFails(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	c, ws := newCleanupTestDispatcher(t, WorkspacePersistCleanupOnDone, wsRoot)

	issueID := "fake:cleanup-2"
	wsPath, _, err := ws.Create(issueID)
	if err != nil {
		t.Fatalf("ws.Create: %v", err)
	}
	entry := &runningEntry{IssueID: issueID, Identifier: "fake#cleanup-2", WorkspacePath: wsPath}
	hook := &Hook{Script: "exit 3"} // non-zero → Hook.Run returns an error

	c.cleanupWorkspace(entry, hook, c.dispatchEnv(entry, DispatchSpec{WorkspacePath: wsPath}))

	if _, err := os.Stat(wsPath); !os.IsNotExist(err) {
		t.Fatalf("workspace %q not removed after a failing before_remove (stat err=%v)", wsPath, err)
	}
}

// TestCleanupWorkspace_SkippedUnderKeepPolicy asserts the default policy is
// untouched: neither the hook nor the removal fires when persist=keep.
func TestCleanupWorkspace_SkippedUnderKeepPolicy(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	c, ws := newCleanupTestDispatcher(t, WorkspacePersistKeep, wsRoot)

	issueID := "fake:cleanup-3"
	wsPath, _, err := ws.Create(issueID)
	if err != nil {
		t.Fatalf("ws.Create: %v", err)
	}
	sentinel := filepath.Join(dir, "should_not_exist")
	hook := &Hook{Script: fmt.Sprintf(`printf ran > %q`, sentinel)}
	entry := &runningEntry{IssueID: issueID, Identifier: "fake#cleanup-3", WorkspacePath: wsPath}

	c.cleanupWorkspace(entry, hook, c.dispatchEnv(entry, DispatchSpec{WorkspacePath: wsPath}))

	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("before_remove ran under persist=keep — cleanup must be a no-op")
	}
	if _, err := os.Stat(wsPath); err != nil {
		t.Fatalf("workspace removed under persist=keep — it must be retained: %v", err)
	}
}

// TestFinishRun_DefersWorkspaceTeardownToWorker locks in that workspace
// teardown is NOT done on the actor goroutine. A clean finishRun (the actor
// path) must leave the directory in place — removal (and the potentially
// slow before_remove shell hook) happens in runWorker, off the actor, so it
// can never block polling/dispatch/snapshots.
func TestFinishRun_DefersWorkspaceTeardownToWorker(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	c, ws := newCleanupTestDispatcher(t, WorkspacePersistCleanupOnDone, wsRoot)

	issueID := "fake:cleanup-4"
	wsPath, _, err := ws.Create(issueID)
	if err != nil {
		t.Fatalf("ws.Create: %v", err)
	}
	c.tracker.(*fakeTracker).add(tracker.Issue{
		ID: issueID, Identifier: "fake#cleanup-4", WorkflowState: "in_progress",
	})
	c.state.running[issueID] = &runningEntry{
		IssueID:       issueID,
		Identifier:    "fake#cleanup-4",
		RunID:         "run-cleanup-4",
		WorkflowState: "in_progress",
		WorkspacePath: wsPath,
	}

	c.finishRun(context.Background(), issueID, nil)

	if _, err := os.Stat(wsPath); err != nil {
		t.Fatalf("finishRun deleted the workspace on the actor path (stat err=%v) — teardown must defer to the worker", err)
	}
}

// TestCleanupWorkspace_KeepsDirtyGitWorkspace is the stranded-work guard:
// when the external workspace is a git checkout with UNCOMMITTED changes
// (a bot without `worktree: auto` that edited but never committed), cleanup
// must be skipped — the before_remove hook must NOT fire and the directory
// must survive, so the operator can recover the work by hand. Destroying it
// (git worktree remove --force + rmdir) would lose finished work forever.
func TestCleanupWorkspace_KeepsDirtyGitWorkspace(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	c, ws := newCleanupTestDispatcher(t, WorkspacePersistCleanupOnDone, wsRoot)

	issueID := "fake:cleanup-dirty"
	wsPath, _, err := ws.Create(issueID)
	if err != nil {
		t.Fatalf("ws.Create: %v", err)
	}
	// Make the workspace a git checkout with uncommitted changes.
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wsPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(wsPath, "uncommitted.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write uncommitted file: %v", err)
	}

	sentinel := filepath.Join(dir, "before_remove_ran")
	hook := &Hook{Script: fmt.Sprintf(`printf ran > %q`, sentinel)}
	entry := &runningEntry{IssueID: issueID, Identifier: "fake#cleanup-dirty", WorkspacePath: wsPath}

	c.cleanupWorkspace(entry, hook, c.dispatchEnv(entry, DispatchSpec{WorkspacePath: wsPath}))

	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("before_remove ran on a dirty workspace — teardown must be skipped to preserve uncommitted work")
	}
	if _, err := os.Stat(filepath.Join(wsPath, "uncommitted.go")); err != nil {
		t.Fatalf("dirty workspace was destroyed (uncommitted.go gone: %v) — it must be preserved for recovery", err)
	}
}

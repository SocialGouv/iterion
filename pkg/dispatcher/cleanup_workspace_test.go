package dispatcher

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	runID := "run-cleanup-1"
	wsPath, _, err := ws.CreateForRun(issueID, runID)
	if err != nil {
		t.Fatalf("ws.CreateForRun: %v", err)
	}

	// before_remove records — into a sentinel OUTSIDE the workspace —
	// whether $ITERION_WORKSPACE still existed when the hook ran. The file
	// is written only if the hook ran AND the directory was still present;
	// its absence means the hook never fired (the original bug).
	sentinel := filepath.Join(dir, "before_remove_saw")
	hook := &Hook{Script: fmt.Sprintf(
		`if [ -d "$ITERION_WORKSPACE" ]; then printf '%%s' "$ITERION_WORKSPACE" > %q; fi`, sentinel)}

	entry := &runningEntry{
		IssueID:                   issueID,
		Identifier:                "fake#cleanup-1",
		RunID:                     runID,
		WorkspaceGeneration:       runID,
		CleanupWorkspaceOnSuccess: true,
		WorkflowState:             "in_progress",
		WorkspacePath:             wsPath,
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
	runID := "run-cleanup-2"
	wsPath, _, err := ws.CreateForRun(issueID, runID)
	if err != nil {
		t.Fatalf("ws.CreateForRun: %v", err)
	}
	entry := &runningEntry{
		IssueID: issueID, Identifier: "fake#cleanup-2", RunID: runID,
		WorkspaceGeneration: runID, CleanupWorkspaceOnSuccess: true, WorkspacePath: wsPath,
	}
	hook := &Hook{Script: "exit 3"} // non-zero → Hook.Run returns an error

	c.cleanupWorkspace(entry, hook, c.dispatchEnv(entry, DispatchSpec{WorkspacePath: wsPath}))

	if _, err := os.Stat(wsPath); !os.IsNotExist(err) {
		t.Fatalf("workspace %q not removed after a failing before_remove (stat err=%v)", wsPath, err)
	}
}

func TestCleanupWorkspace_RetireFailurePreservesWorkspaceAndSkipsHook(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	c, ws := newCleanupTestDispatcher(t, WorkspacePersistCleanupOnDone, wsRoot)

	issueID := "fake:cleanup-retire-failure"
	runID := "run-cleanup-retire-failure"
	wsPath, _, err := ws.CreateForRun(issueID, runID)
	if err != nil {
		t.Fatalf("ws.CreateForRun: %v", err)
	}
	if err := os.WriteFile(ws.ownerPathForRun(issueID, runID), []byte("{broken"), 0o600); err != nil {
		t.Fatalf("corrupt ownership marker: %v", err)
	}

	sentinel := filepath.Join(dir, "before_remove_must_not_run")
	hook := &Hook{Script: fmt.Sprintf(`printf ran > %q`, sentinel)}
	entry := &runningEntry{
		IssueID: issueID, Identifier: "fake#cleanup-retire-failure", RunID: runID,
		WorkspaceGeneration: runID, CleanupWorkspaceOnSuccess: true, WorkspacePath: wsPath,
	}
	c.cleanupWorkspace(entry, hook, c.dispatchEnv(entry, DispatchSpec{WorkspacePath: wsPath}))

	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("before_remove ran without a valid retirement authority")
	}
	if _, err := os.Stat(wsPath); err != nil {
		t.Fatalf("workspace changed after retirement failure: %v", err)
	}
}

func TestCleanupWorkspace_CleansStableWorkspaceFromResumedOlderRun(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	c, ws := newCleanupTestDispatcher(t, WorkspacePersistCleanupOnDone, wsRoot)

	issueID := "fake:cleanup-stable-resume"
	runID := "run-cleanup-stable-resume"
	wsPath, _, err := ws.Create(issueID)
	if err != nil {
		t.Fatalf("ws.Create: %v", err)
	}
	entry := &runningEntry{
		IssueID: issueID, Identifier: "fake#cleanup-stable-resume", RunID: runID,
		CleanupWorkspaceOnSuccess: true, WorkspacePath: wsPath,
	}

	c.cleanupWorkspace(entry, nil, c.dispatchEnv(entry, DispatchSpec{WorkspacePath: wsPath}))

	if _, err := os.Stat(wsPath); !os.IsNotExist(err) {
		t.Fatalf("stable resumed workspace %q not removed (stat err=%v)", wsPath, err)
	}
}

// TestCleanupWorkspace_RemovesLinkedWorktreeRegistration covers the default
// dispatch configuration's lifecycle: after_create uses `git worktree add`
// to seed the owned directory, while cleanup deliberately has no destructive
// before_remove hook. Once the directory is gone its host-repository
// registration must be removed before a later generation is seeded.
// Deregistration is exact: another missing checkout in the same repository
// remains registered for its own recovery path.
func TestCleanupWorkspace_RemovesLinkedWorktreeRegistration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	hostRepo := filepath.Join(dir, "host")
	if err := os.Mkdir(hostRepo, 0o755); err != nil {
		t.Fatalf("mkdir host repo: %v", err)
	}
	runCleanupGit(t, hostRepo, "init", "-b", "main")
	runCleanupGit(t, hostRepo, "config", "user.email", "test@example.com")
	runCleanupGit(t, hostRepo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(hostRepo, "README.md"), []byte("host\n"), 0o644); err != nil {
		t.Fatalf("write host fixture: %v", err)
	}
	runCleanupGit(t, hostRepo, "add", "README.md")
	runCleanupGit(t, hostRepo, "commit", "-m", "initial")

	wsRoot := filepath.Join(dir, "ws")
	c, ws := newCleanupTestDispatcher(t, WorkspacePersistCleanupOnDone, wsRoot)
	issueID := "fake:linked-worktree"
	runID := "run-linked-worktree"
	wsPath, _, err := ws.CreateForRun(issueID, runID)
	if err != nil {
		t.Fatalf("ws.CreateForRun: %v", err)
	}
	runCleanupGit(t, hostRepo, "worktree", "add", "--detach", wsPath, "HEAD")

	otherPath := filepath.Join(dir, "other-missing-worktree")
	runCleanupGit(t, hostRepo, "worktree", "add", "--detach", otherPath, "HEAD")
	if err := os.RemoveAll(otherPath); err != nil {
		t.Fatalf("remove unrelated checkout fixture: %v", err)
	}

	entry := &runningEntry{
		IssueID:                   issueID,
		Identifier:                "fake#linked-worktree",
		RunID:                     runID,
		WorkspaceGeneration:       runID,
		CleanupWorkspaceOnSuccess: true,
		WorkspacePath:             wsPath,
	}
	c.cleanupWorkspace(entry, nil, c.dispatchEnv(entry, DispatchSpec{
		RunID:         entry.RunID,
		WorkspacePath: wsPath,
	}))

	if _, err := os.Stat(wsPath); !os.IsNotExist(err) {
		t.Fatalf("workspace %q not removed (stat err=%v)", wsPath, err)
	}
	if listed := runCleanupGit(t, hostRepo, "worktree", "list", "--porcelain"); strings.Contains(listed, wsPath) {
		t.Fatalf("deleted workspace still registered in host repo:\n%s", listed)
	}
	if listed := runCleanupGit(t, hostRepo, "worktree", "list", "--porcelain"); !strings.Contains(listed, otherPath) {
		t.Fatalf("cleanup removed unrelated missing worktree registration:\n%s", listed)
	}

	// A later logical dispatch gets a new absolute path, so a stale writer
	// cannot contaminate it; exact deregistration also lets Git seed it.
	recreatedPath, created, err := ws.CreateForRun(issueID, "run-linked-next")
	if err != nil {
		t.Fatalf("ws.CreateForRun after cleanup: %v", err)
	}
	if !created || recreatedPath == wsPath {
		t.Fatalf("next workspace = (%q, %t), must differ from retired %q", recreatedPath, created, wsPath)
	}
	runCleanupGit(t, hostRepo, "worktree", "add", "--detach", recreatedPath, "HEAD")
}

// TestCleanupWorkspace_SkippedUnderKeepPolicy asserts the default policy is
// untouched: neither the hook nor the removal fires when persist=keep.
func TestCleanupWorkspace_SkippedUnderKeepPolicy(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	c, ws := newCleanupTestDispatcher(t, WorkspacePersistKeep, wsRoot)

	issueID := "fake:cleanup-3"
	runID := "run-cleanup-3"
	wsPath, _, err := ws.Create(issueID)
	if err != nil {
		t.Fatalf("ws.Create: %v", err)
	}
	sentinel := filepath.Join(dir, "should_not_exist")
	hook := &Hook{Script: fmt.Sprintf(`printf ran > %q`, sentinel)}
	entry := &runningEntry{
		IssueID: issueID, Identifier: "fake#cleanup-3", RunID: runID,
		WorkspacePath: wsPath,
	}

	c.cleanupWorkspace(entry, hook, c.dispatchEnv(entry, DispatchSpec{WorkspacePath: wsPath}))

	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("before_remove ran under persist=keep — cleanup must be a no-op")
	}
	if _, err := os.Stat(wsPath); err != nil {
		t.Fatalf("workspace removed under persist=keep — it must be retained: %v", err)
	}
	if reused, created, err := ws.Create(issueID); err != nil || created || reused != wsPath {
		t.Fatalf("persist=keep lost stable workspace reuse: path=%q created=%t err=%v", reused, created, err)
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
	runID := "run-cleanup-4"
	wsPath, _, err := ws.CreateForRun(issueID, runID)
	if err != nil {
		t.Fatalf("ws.CreateForRun: %v", err)
	}
	c.tracker.(*fakeTracker).add(tracker.Issue{
		ID: issueID, Identifier: "fake#cleanup-4", WorkflowState: "in_progress",
	})
	c.state.running[issueID] = &runningEntry{
		IssueID:             issueID,
		Identifier:          "fake#cleanup-4",
		RunID:               runID,
		WorkspaceGeneration: runID,
		WorkflowState:       "in_progress",
		WorkspacePath:       wsPath,
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
	runID := "run-cleanup-dirty"
	wsPath, _, err := ws.CreateForRun(issueID, runID)
	if err != nil {
		t.Fatalf("ws.CreateForRun: %v", err)
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
	entry := &runningEntry{
		IssueID: issueID, Identifier: "fake#cleanup-dirty", RunID: runID,
		WorkspaceGeneration: runID, CleanupWorkspaceOnSuccess: true, WorkspacePath: wsPath,
	}

	c.cleanupWorkspace(entry, hook, c.dispatchEnv(entry, DispatchSpec{WorkspacePath: wsPath}))

	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("before_remove ran on a dirty workspace — teardown must be skipped to preserve uncommitted work")
	}
	if _, err := os.Stat(filepath.Join(wsPath, "uncommitted.go")); err != nil {
		t.Fatalf("dirty workspace was destroyed (uncommitted.go gone: %v) — it must be preserved for recovery", err)
	}
	if samePath, created, err := ws.CreateForRun(issueID, runID); err != nil || created || samePath != wsPath {
		t.Fatalf("dirty workspace ownership was retired: path=%q created=%t err=%v", samePath, created, err)
	}
}

func runCleanupGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

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
	"github.com/SocialGouv/iterion/pkg/store"
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
		StoreDir:   filepath.Join(t.TempDir(), "store"),
		HostMarker: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, ws
}

type cleanupGitFixture struct {
	repo string
	path string
	base string
	run  *store.Run
}

func newCleanupGitFixture(
	t *testing.T,
	c *Dispatcher,
	ws *Workspaces,
	issueID string,
	runID string,
	ownership store.WorktreeOwnership,
) cleanupGitFixture {
	t.Helper()
	repo := t.TempDir()
	cleanupGit(t, repo, "init", "-b", "main")
	cleanupGit(t, repo, "config", "user.email", "test@example.com")
	cleanupGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write tracked fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("generated/\nnode_modules/\n"), 0o644); err != nil {
		t.Fatalf("write gitignore fixture: %v", err)
	}
	cleanupGit(t, repo, "add", ".")
	cleanupGit(t, repo, "commit", "-m", "base")
	base := cleanupGit(t, repo, "rev-parse", "HEAD")

	path, _, err := ws.CreateForRun(issueID, runID)
	if err != nil {
		t.Fatalf("ws.Create: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove empty workspace before git worktree add: %v", err)
	}
	cleanupGit(t, repo, "worktree", "add", "--detach", path, base)

	run := &store.Run{
		FormatVersion:     store.RunFormatVersion,
		ID:                runID,
		Status:            store.RunStatusFinished,
		Worktree:          true,
		WorktreeOwnership: ownership,
		RepoRoot:          repo,
		BaseCommit:        base,
	}
	switch ownership {
	case store.WorktreeOwnershipManaged:
		run.WorkDir = filepath.Join(c.storeDir, "worktrees", runID)
	case store.WorktreeOwnershipDelegated:
		run.WorkDir = path
		run.WorktreeGitDir = cleanupGit(t, path, "rev-parse", "--absolute-git-dir")
	default:
		t.Fatalf("unsupported cleanup fixture ownership %q", ownership)
	}
	saveCleanupRun(t, c, run)
	return cleanupGitFixture{repo: repo, path: path, base: base, run: run}
}

func saveCleanupRun(t *testing.T, c *Dispatcher, run *store.Run) {
	t.Helper()
	st, err := store.New(c.storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
}

func cleanupGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestCleanupWorkspace_RunsBeforeRemoveBeforeDeletingDir is the core
// regression guard for the "before_remove is never invoked" bug. Under a
// cleanup policy, cleanupWorkspace must run the before_remove hook AND it
// must run while the workspace still exists, then the dispatcher must
// deregister and remove the linked worktree through the safe runtime path.
func TestCleanupWorkspace_RunsBeforeRemoveBeforeDeletingDir(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	c, ws := newCleanupTestDispatcher(t, WorkspacePersistCleanupOnDone, wsRoot)

	issueID := "fake:cleanup-1"
	runID := "run-cleanup-1"
	fixture := newCleanupGitFixture(t, c, ws, issueID, runID, store.WorktreeOwnershipManaged)
	wsPath := fixture.path

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
		RunID:         runID,
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
	if _, _, err := ws.CreateForRun(issueID, runID); err == nil {
		t.Fatal("retired run generation became reusable after cleanup")
	}
	if fresh, created, err := ws.CreateForRun(issueID, runID+"-next"); err != nil {
		t.Fatalf("new run generation could not create a workspace: %v", err)
	} else if !created || fresh == wsPath {
		t.Fatalf("new generation workspace = (%q, created=%t), want a fresh path distinct from %q", fresh, created, wsPath)
	}
}

// TestCleanupWorkspace_PreservesWhenBeforeRemoveFails asserts the fail-closed
// contract: a partial or failing operator hook never authorizes a subsequent
// recursive deletion.
func TestCleanupWorkspace_PreservesWhenBeforeRemoveFails(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	c, ws := newCleanupTestDispatcher(t, WorkspacePersistCleanupOnDone, wsRoot)

	issueID := "fake:cleanup-2"
	runID := "run-cleanup-2"
	fixture := newCleanupGitFixture(t, c, ws, issueID, runID, store.WorktreeOwnershipManaged)
	wsPath := fixture.path
	entry := &runningEntry{
		IssueID: issueID, Identifier: "fake#cleanup-2", RunID: runID, WorkspacePath: wsPath,
	}
	hook := &Hook{Script: "exit 3"} // non-zero → Hook.Run returns an error

	c.cleanupWorkspace(entry, hook, c.dispatchEnv(entry, DispatchSpec{RunID: runID, WorkspacePath: wsPath}))

	if _, err := os.Stat(wsPath); err != nil {
		t.Fatalf("workspace %q removed after a failing before_remove: %v", wsPath, err)
	}
	if _, _, err := ws.CreateForRun(issueID, runID); err == nil {
		t.Fatal("failing before_remove left the preserved workspace reusable")
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
// would lose finished work forever.
func TestCleanupWorkspace_KeepsDirtyGitWorkspace(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "ws")
	c, ws := newCleanupTestDispatcher(t, WorkspacePersistCleanupOnDone, wsRoot)

	issueID := "fake:cleanup-dirty"
	runID := "run-cleanup-dirty"
	fixture := newCleanupGitFixture(t, c, ws, issueID, runID, store.WorktreeOwnershipManaged)
	wsPath := fixture.path
	if err := os.WriteFile(filepath.Join(wsPath, "uncommitted.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write uncommitted file: %v", err)
	}

	sentinel := filepath.Join(dir, "before_remove_ran")
	hook := &Hook{Script: fmt.Sprintf(`printf ran > %q`, sentinel)}
	entry := &runningEntry{
		IssueID: issueID, Identifier: "fake#cleanup-dirty", RunID: runID, WorkspacePath: wsPath,
	}

	c.cleanupWorkspace(entry, hook, c.dispatchEnv(entry, DispatchSpec{RunID: runID, WorkspacePath: wsPath}))

	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("before_remove ran on a dirty workspace — teardown must be skipped to preserve uncommitted work")
	}
	if _, err := os.Stat(filepath.Join(wsPath, "uncommitted.go")); err != nil {
		t.Fatalf("dirty workspace was destroyed (uncommitted.go gone: %v) — it must be preserved for recovery", err)
	}
}

func TestCleanupWorkspace_RemovesExactlyFinalizedDelegatedWorktree(t *testing.T) {
	dir := t.TempDir()
	c, ws := newCleanupTestDispatcher(
		t,
		WorkspacePersistCleanupOnDone,
		filepath.Join(dir, "ws"),
	)
	issueID := "fake:cleanup-delegated"
	runID := "run-cleanup-delegated"
	fixture := newCleanupGitFixture(t, c, ws, issueID, runID, store.WorktreeOwnershipDelegated)

	if err := os.WriteFile(filepath.Join(fixture.path, "result.txt"), []byte("durable\n"), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}
	cleanupGit(t, fixture.path, "add", "result.txt")
	cleanupGit(t, fixture.path, "commit", "-m", "result")
	finalCommit := cleanupGit(t, fixture.path, "rev-parse", "HEAD")
	finalBranch := "iterion/run/cleanup-delegated"
	cleanupGit(t, fixture.repo, "branch", finalBranch, finalCommit)
	fixture.run.FinalCommit = finalCommit
	fixture.run.FinalBranch = finalBranch
	saveCleanupRun(t, c, fixture.run)

	entry := &runningEntry{
		IssueID: issueID, Identifier: "fake#cleanup-delegated", RunID: runID, WorkspacePath: fixture.path,
	}
	c.cleanupWorkspace(entry, nil, c.dispatchEnv(entry, DispatchSpec{RunID: runID, WorkspacePath: fixture.path}))

	if _, err := os.Stat(fixture.path); !os.IsNotExist(err) {
		t.Fatalf("verified delegated worktree was not removed (stat err=%v)", err)
	}
	if got := cleanupGit(t, fixture.repo, "rev-parse", finalBranch); got != finalCommit {
		t.Fatalf("durable branch moved: got %s, want %s", got, finalCommit)
	}
}

func TestCleanupWorkspace_PreservesCleanCommitCreatedAfterFinalization(t *testing.T) {
	dir := t.TempDir()
	c, ws := newCleanupTestDispatcher(
		t,
		WorkspacePersistCleanupOnDone,
		filepath.Join(dir, "ws"),
	)
	issueID := "fake:cleanup-new-head"
	runID := "run-cleanup-new-head"
	fixture := newCleanupGitFixture(t, c, ws, issueID, runID, store.WorktreeOwnershipDelegated)

	if err := os.WriteFile(filepath.Join(fixture.path, "result.txt"), []byte("finalized\n"), 0o644); err != nil {
		t.Fatalf("write finalized result: %v", err)
	}
	cleanupGit(t, fixture.path, "add", "result.txt")
	cleanupGit(t, fixture.path, "commit", "-m", "finalized")
	finalCommit := cleanupGit(t, fixture.path, "rev-parse", "HEAD")
	fixture.run.FinalCommit = finalCommit
	fixture.run.FinalBranch = "iterion/run/cleanup-new-head"
	cleanupGit(t, fixture.repo, "branch", fixture.run.FinalBranch, finalCommit)
	saveCleanupRun(t, c, fixture.run)

	if err := os.WriteFile(filepath.Join(fixture.path, "after-run.txt"), []byte("late commit\n"), 0o644); err != nil {
		t.Fatalf("write after-run result: %v", err)
	}
	cleanupGit(t, fixture.path, "add", "after-run.txt")
	cleanupGit(t, fixture.path, "commit", "-m", "after-run")
	lateCommit := cleanupGit(t, fixture.path, "rev-parse", "HEAD")

	sentinel := filepath.Join(dir, "hook-ran")
	hook := &Hook{Script: fmt.Sprintf(`printf ran > %q`, sentinel)}
	entry := &runningEntry{
		IssueID: issueID, Identifier: "fake#cleanup-new-head", RunID: runID, WorkspacePath: fixture.path,
	}
	c.cleanupWorkspace(entry, hook, c.dispatchEnv(entry, DispatchSpec{RunID: runID, WorkspacePath: fixture.path}))

	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("before_remove ran even though HEAD no longer matched finalized metadata")
	}
	if got := cleanupGit(t, fixture.path, "rev-parse", "HEAD"); got != lateCommit {
		t.Fatalf("late clean commit was lost: got HEAD %s, want %s", got, lateCommit)
	}
}

func TestCleanupWorkspace_RechecksAfterBeforeRemoveHook(t *testing.T) {
	dir := t.TempDir()
	c, ws := newCleanupTestDispatcher(
		t,
		WorkspacePersistCleanupOnDone,
		filepath.Join(dir, "ws"),
	)
	issueID := "fake:cleanup-hook-head"
	runID := "run-cleanup-hook-head"
	fixture := newCleanupGitFixture(t, c, ws, issueID, runID, store.WorktreeOwnershipDelegated)

	hook := &Hook{Script: `
printf 'hook commit\n' > hook-result.txt
git add hook-result.txt
git commit -m 'before-remove result'
`}
	entry := &runningEntry{
		IssueID: issueID, Identifier: "fake#cleanup-hook-head", RunID: runID, WorkspacePath: fixture.path,
	}
	c.cleanupWorkspace(entry, hook, c.dispatchEnv(entry, DispatchSpec{RunID: runID, WorkspacePath: fixture.path}))

	if _, err := os.Stat(filepath.Join(fixture.path, "hook-result.txt")); err != nil {
		t.Fatalf("hook-created result was deleted: %v", err)
	}
	if got := cleanupGit(t, fixture.path, "rev-parse", "HEAD"); got == fixture.base {
		t.Fatalf("hook did not create the clean commit needed by the test")
	}
}

func TestCleanupWorkspace_PreservesIgnoredNonDisposableOutput(t *testing.T) {
	dir := t.TempDir()
	c, ws := newCleanupTestDispatcher(
		t,
		WorkspacePersistCleanupOnDone,
		filepath.Join(dir, "ws"),
	)
	issueID := "fake:cleanup-ignored"
	runID := "run-cleanup-ignored"
	fixture := newCleanupGitFixture(t, c, ws, issueID, runID, store.WorktreeOwnershipDelegated)

	generatedDir := filepath.Join(fixture.path, "generated")
	if err := os.MkdirAll(generatedDir, 0o755); err != nil {
		t.Fatalf("mkdir ignored output: %v", err)
	}
	outputPath := filepath.Join(generatedDir, "report.json")
	if err := os.WriteFile(outputPath, []byte(`{"valuable":true}`), 0o644); err != nil {
		t.Fatalf("write ignored output: %v", err)
	}

	sentinel := filepath.Join(dir, "hook-ran")
	hook := &Hook{Script: fmt.Sprintf(`printf ran > %q`, sentinel)}
	entry := &runningEntry{
		IssueID: issueID, Identifier: "fake#cleanup-ignored", RunID: runID, WorkspacePath: fixture.path,
	}
	c.cleanupWorkspace(entry, hook, c.dispatchEnv(entry, DispatchSpec{RunID: runID, WorkspacePath: fixture.path}))

	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("before_remove ran despite protected ignored output")
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("ignored output was deleted: %v", err)
	}
}

func TestCleanupWorkspace_PreservesWhenRunMetadataMissing(t *testing.T) {
	dir := t.TempDir()
	c, ws := newCleanupTestDispatcher(
		t,
		WorkspacePersistCleanupOnDone,
		filepath.Join(dir, "ws"),
	)
	issueID := "fake:cleanup-no-metadata"
	runID := "run-cleanup-no-metadata"
	fixture := newCleanupGitFixture(t, c, ws, issueID, runID, store.WorktreeOwnershipManaged)
	runJSON := filepath.Join(c.storeDir, "runs", runID, "run.json")
	if err := os.Remove(runJSON); err != nil {
		t.Fatalf("remove run metadata fixture: %v", err)
	}

	sentinel := filepath.Join(dir, "hook-ran")
	hook := &Hook{Script: fmt.Sprintf(`printf ran > %q`, sentinel)}
	entry := &runningEntry{
		IssueID: issueID, Identifier: "fake#cleanup-no-metadata", RunID: runID, WorkspacePath: fixture.path,
	}
	c.cleanupWorkspace(entry, hook, c.dispatchEnv(entry, DispatchSpec{RunID: runID, WorkspacePath: fixture.path}))

	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("before_remove ran without readable run metadata")
	}
	if _, err := os.Stat(fixture.path); err != nil {
		t.Fatalf("workspace was removed without persisted proof: %v", err)
	}
}

func TestCleanupWorkspace_PreservesUnprotectedFinalCommit(t *testing.T) {
	dir := t.TempDir()
	c, ws := newCleanupTestDispatcher(
		t,
		WorkspacePersistCleanupOnDone,
		filepath.Join(dir, "ws"),
	)
	issueID := "fake:cleanup-unprotected"
	runID := "run-cleanup-unprotected"
	fixture := newCleanupGitFixture(t, c, ws, issueID, runID, store.WorktreeOwnershipDelegated)

	if err := os.WriteFile(filepath.Join(fixture.path, "result.txt"), []byte("unprotected\n"), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}
	cleanupGit(t, fixture.path, "add", "result.txt")
	cleanupGit(t, fixture.path, "commit", "-m", "unprotected")
	finalCommit := cleanupGit(t, fixture.path, "rev-parse", "HEAD")
	fixture.run.FinalCommit = finalCommit
	fixture.run.FinalBranch = ""
	saveCleanupRun(t, c, fixture.run)

	entry := &runningEntry{
		IssueID: issueID, Identifier: "fake#cleanup-unprotected", RunID: runID, WorkspacePath: fixture.path,
	}
	c.cleanupWorkspace(entry, nil, c.dispatchEnv(entry, DispatchSpec{RunID: runID, WorkspacePath: fixture.path}))

	if got := cleanupGit(t, fixture.path, "rev-parse", "HEAD"); got != finalCommit {
		t.Fatalf("unprotected commit was lost: got %s, want %s", got, finalCommit)
	}
}

func TestCleanupWorkspace_PreservesCorruptManagedOwnership(t *testing.T) {
	dir := t.TempDir()
	c, ws := newCleanupTestDispatcher(
		t,
		WorkspacePersistCleanupOnDone,
		filepath.Join(dir, "ws"),
	)
	issueID := "fake:cleanup-corrupt-managed"
	runID := "run-cleanup-corrupt-managed"
	fixture := newCleanupGitFixture(t, c, ws, issueID, runID, store.WorktreeOwnershipManaged)
	fixture.run.WorkDir = filepath.Join(t.TempDir(), "foreign-worktree")
	saveCleanupRun(t, c, fixture.run)

	sentinel := filepath.Join(dir, "hook-ran")
	hook := &Hook{Script: fmt.Sprintf(`printf ran > %q`, sentinel)}
	entry := &runningEntry{
		IssueID: issueID, Identifier: "fake#cleanup-corrupt-managed", RunID: runID, WorkspacePath: fixture.path,
	}
	c.cleanupWorkspace(entry, hook, c.dispatchEnv(entry, DispatchSpec{RunID: runID, WorkspacePath: fixture.path}))

	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("before_remove ran with corrupt managed ownership metadata")
	}
	if _, err := os.Stat(fixture.path); err != nil {
		t.Fatalf("workspace was removed with corrupt managed ownership metadata: %v", err)
	}
}

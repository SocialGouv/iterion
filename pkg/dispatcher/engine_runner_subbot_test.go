package dispatcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The child is tool-only (no API keys) and reads a file by RELATIVE path, so
// it also proves the child inherited the dispatcher's per-issue workspace as
// its working directory: from the daemon's own cwd the `cat` finds nothing.
const subbotChildBot = `
schema child_out:
  validated: bool
  echoed: string

tool work:
  command: "cat marker.json"
  output: child_out

workflow subbot_child:
  entry: work
  work -> done
`

const subbotParentBot = `
schema seed_out:
  ok: bool

schema child_out:
  validated: bool
  echoed: string

compute seed:
  output: seed_out
  expr:
    ok: "true"

subbot run_child:
  source: "child.bot"
  output: child_out

workflow subbot_dispatch_demo:
  entry: seed
  seed      -> run_child
  run_child -> done when validated
  run_child -> fail
`

// TestEngineRunner_DispatchRunsSubbot pins the runner the dispatcher's direct
// engine path lacked: every `subbot` node of a dispatched bot used to die with
// "no SubbotRunner is wired" — the CLI and the studio each wired one, this
// path never did, and its own retries re-enter the same engine so they were no
// escape either. The child's relative-path `cat` doubles as the workspace
// assertion: the daemon's cwd is the host repo, so a child that did not
// inherit spec.WorkspacePath would read the wrong tree.
func TestEngineRunner_DispatchRunsSubbot(t *testing.T) {
	botDir := t.TempDir()
	parentPath := filepath.Join(botDir, "parent.bot")
	if err := os.WriteFile(parentPath, []byte(subbotParentBot), 0o644); err != nil {
		t.Fatalf("write parent bot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "child.bot"), []byte(subbotChildBot), 0o644); err != nil {
		t.Fatalf("write child bot: %v", err)
	}

	workspace := t.TempDir()
	marker := `{"validated":true,"echoed":"from-workspace"}`
	if err := os.WriteFile(filepath.Join(workspace, "marker.json"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	storeDir := t.TempDir()
	runner, err := NewEngineRunner(parentPath, iterlog.Nop())
	if err != nil {
		t.Fatalf("NewEngineRunner: %v", err)
	}
	defer func() { _ = runner.Close() }()

	runID, err := store.GenerateRunID()
	if err != nil {
		t.Fatalf("GenerateRunID: %v", err)
	}
	derr := runner.Dispatch(context.Background(), DispatchSpec{
		RunID:         runID,
		WorkspacePath: workspace,
		StoreDir:      storeDir,
		Issue:         &IssueRef{ID: "native:" + runID, Identifier: runID, Title: "subbot under dispatch"},
	})
	if derr != nil {
		if strings.Contains(derr.Error(), "no SubbotRunner is wired") {
			t.Fatalf("the dispatcher engine still has no SubbotRunner: %v", derr)
		}
		t.Fatalf("Dispatch: %v", derr)
	}

	s, err := store.New(storeDir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("parent status = %q, want finished (error: %s)", r.Status, r.Error)
	}

	// The child ran as its OWN run, linked back to the parent — that linkage is
	// what folds it into the parent's pipeline-board card.
	ids, err := s.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	var child *store.Run
	for _, id := range ids {
		if id == runID {
			continue
		}
		candidate, lerr := s.LoadRun(context.Background(), id)
		if lerr != nil {
			t.Fatalf("LoadRun(%s): %v", id, lerr)
		}
		if candidate.ParentRunID == runID {
			child = candidate
			break
		}
	}
	if child == nil {
		t.Fatalf("no child run linked to parent %s — the subbot node did not spawn one", runID)
	}
	if child.Status != store.RunStatusFinished {
		t.Fatalf("child status = %q, want finished (error: %s)", child.Status, child.Error)
	}
}

// slowChildBot idles long enough for the test to probe the child's run lock
// while its active pass is genuinely in flight.
// The child BLOCKS mid-pass until the test releases it by touching
// release-lock-probe in the shared workspace. A fixed sleep raced the
// probe on loaded CI runners (-race spawn alone can eat many seconds):
// the child finished before ever being observed, and the test failed at
// its deadline having proven nothing.
const slowSubbotChildBot = `
schema child_out:
  validated: bool
  echoed: string

tool work:
  command: "while [ ! -f release-lock-probe ]; do sleep 0.1; done; cat marker.json"
  output: child_out

workflow subbot_child:
  entry: work
  work -> done
`

// TestEngineRunner_SubbotChildHoldsRunLock pins the liveness signal a
// dispatcher-spawned child has and nothing else gives it. runview's orphan
// reaper probes LockRun: an unlocked `running` run reads as dead. A child here
// is neither in runview's Manager (the studio's runner registers one; there is
// nothing to register with on this path) nor a detached-.pid runner, so without
// its own flock a reconcile tick flips a WORKING child to failed_resumable.
// That is not hypothetical — it happened on the first dispatched run to reach a
// subbot, which survived only because its human-gate write landed 16s behind
// the reap and won. A Blender render or a video generation would not.
//
// The lock must also be GONE once the active pass returns: a child parked on a
// human gate is resumed externally, and that resume takes the same lock.
func TestEngineRunner_SubbotChildHoldsRunLock(t *testing.T) {
	botDir := t.TempDir()
	parentPath := filepath.Join(botDir, "parent.bot")
	if err := os.WriteFile(parentPath, []byte(subbotParentBot), 0o644); err != nil {
		t.Fatalf("write parent bot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "child.bot"), []byte(slowSubbotChildBot), 0o644); err != nil {
		t.Fatalf("write child bot: %v", err)
	}
	workspace := t.TempDir()
	marker := `{"validated":true,"echoed":"from-workspace"}`
	if err := os.WriteFile(filepath.Join(workspace, "marker.json"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	storeDir := t.TempDir()
	probe, err := store.New(storeDir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if !probe.Capabilities().CrossProcessLock {
		t.Skip("store has no cross-process lock — LockRun is a noop here, nothing to assert")
	}

	runner, err := NewEngineRunner(parentPath, iterlog.Nop())
	if err != nil {
		t.Fatalf("NewEngineRunner: %v", err)
	}
	defer func() { _ = runner.Close() }()
	runID, err := store.GenerateRunID()
	if err != nil {
		t.Fatalf("GenerateRunID: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- runner.Dispatch(context.Background(), DispatchSpec{
			RunID:         runID,
			WorkspacePath: workspace,
			StoreDir:      storeDir,
			Issue:         &IssueRef{ID: "native:" + runID, Identifier: runID, Title: "subbot lock"},
		})
	}()

	// Catch the child mid-pass and prove its lock is held. The child blocks
	// until release-lock-probe exists, so there is no timing window to race —
	// the deadline only bounds engine spawn on a loaded runner. Cleanup writes
	// the release file even on a Fatal path, so the dispatch goroutine always
	// drains instead of leaking a forever-polling child into the next test.
	release := filepath.Join(workspace, "release-lock-probe")
	t.Cleanup(func() { _ = os.WriteFile(release, []byte("go"), 0o644) })
	childID := ""
	held := false
	// Watch the dispatch goroutine while polling. The child blocks until the
	// release file exists, so Dispatch returning here means it never reached
	// the subbot node — and its error is the diagnosis. Without this the loop
	// runs the deadline out and reports "no child appeared", which names the
	// symptom while the cause sits unread in the channel (observed in CI:
	// 60s burned, nothing actionable in the log).
	var dispatchErr error
	dispatched := false
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && !held {
		select {
		case dispatchErr = <-done:
			dispatched = true
		default:
		}
		if dispatched {
			break
		}
		ids, lerr := probe.ListRuns(context.Background())
		if lerr != nil {
			t.Fatalf("ListRuns: %v", lerr)
		}
		terminal := false
		for _, id := range ids {
			if id == runID {
				continue
			}
			r, rerr := probe.LoadRun(context.Background(), id)
			if rerr != nil || r.ParentRunID != runID {
				continue
			}
			childID = id
			if r.Status != store.RunStatusRunning {
				terminal = r.Status.IsTerminal()
				break
			}
			lock, aerr := probe.LockRun(context.Background(), id)
			if aerr != nil {
				held = true // locked by the child engine — what we came to see
				break
			}
			_ = lock.Unlock()
			after, arr := probe.LoadRun(context.Background(), id)
			if arr == nil && after.Status == store.RunStatusRunning {
				t.Fatalf("child %s is running but its run lock was free — the orphan reaper would read it as dead", id)
			}
			terminal = true // raced the end of the active pass, not a defect
			break
		}
		if terminal {
			break
		}
		if !held {
			time.Sleep(25 * time.Millisecond)
		}
	}
	// Release the child BEFORE asserting: a Fatal below must not leave the
	// dispatch goroutine blocked on a child that waits forever.
	if err := os.WriteFile(release, []byte("go"), 0o644); err != nil {
		t.Fatalf("write release file: %v", err)
	}
	if dispatched && childID == "" {
		t.Fatalf("Dispatch returned before any child run appeared — the subbot node was never reached: %v", dispatchErr)
	}
	if childID == "" {
		t.Fatal("never observed a child run — the subbot node did not spawn one")
	}
	if !held {
		t.Fatal("never caught the child mid-pass — its lock was never observed held while it was blocked on the release file")
	}

	if !dispatched {
		dispatchErr = <-done
	}
	if dispatchErr != nil {
		t.Fatalf("Dispatch: %v", dispatchErr)
	}
	lock, err := probe.LockRun(context.Background(), childID)
	if err != nil {
		t.Fatalf("child lock still held after the active pass — an external resume of a parked child would be blocked: %v", err)
	}
	_ = lock.Unlock()
}

// worktreeNoneParentBot mirrors the film pipeline that surfaced this: the
// PARENT declares `worktree: none` too, so its workDir stays the dispatcher's
// per-issue linked worktree instead of a fresh per-run one — and that is the
// directory the child gets handed, and would have claimed.
const worktreeNoneParentBot = `
schema seed_out:
  ok: bool

schema child_out:
  validated: bool
  echoed: string

compute seed:
  output: seed_out
  expr:
    ok: "true"

subbot run_child:
  source: "child.bot"
  output: child_out

workflow subbot_dispatch_demo:
  worktree: none
  entry: seed
  seed      -> run_child
  run_child -> done when validated
  run_child -> fail
`

// worktreeNoneChildBot is the shape that makes the adoption fire: a child that
// declares `worktree: none`, so it never builds a worktree of its own and the
// workDir it was handed is the only one it has.
const worktreeNoneChildBot = `
schema child_out:
  validated: bool
  echoed: string

tool work:
  command: "cat marker.json"
  output: child_out

workflow subbot_child:
  worktree: none
  entry: work
  work -> done
`

// TestEngineRunner_SubbotChildNeverAdoptsParentWorktree pins the invariant that
// a nested run does not own the workspace it was lent.
//
// `WithWorkDir` sets workDirDelegated, and runPersistWorkspace reads that as
// "this workspace is yours to close": for a `worktree: none` run whose workDir
// is a linked worktree, it stamps Worktree=true + RepoRoot=<main checkout> +
// BaseCommit=<HEAD>. Handing the child its parent's workDir therefore made the
// CHILD claim the directory the PARENT is still writing in — and every resume
// path finalizes unconditionally, so answering the child's human gate would
// `git add -A && git commit` the parent's half-written tree, branch it as
// iterion/run/*, and (with a review gate) squash-merge it into the operator's
// checkout. The step-0 guard does not catch it: it only refuses when
// wtPath == repoRoot, and here they differ.
//
// Observed live before the gate: a paused identity child carried
// Worktree=true with RepoRoot pointing at the operator's own repository.
func TestEngineRunner_SubbotChildNeverAdoptsParentWorktree(t *testing.T) {
	botDir := t.TempDir()
	parentPath := filepath.Join(botDir, "parent.bot")
	if err := os.WriteFile(parentPath, []byte(worktreeNoneParentBot), 0o644); err != nil {
		t.Fatalf("write parent bot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "child.bot"), []byte(worktreeNoneChildBot), 0o644); err != nil {
		t.Fatalf("write child bot: %v", err)
	}

	// The workspace must be a LINKED worktree of a main checkout — that is the
	// shape the dispatcher's after_create hook seeds, and the one the adoption
	// branch tests for (mainRepoRoot != worktreeRoot).
	mainRepo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
		{"commit", "-q", "--allow-empty", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = mainRepo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	workspace := filepath.Join(t.TempDir(), "issue-ws")
	cmd := exec.Command("git", "worktree", "add", "--detach", workspace, "HEAD")
	cmd.Dir = mainRepo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v (%s)", err, out)
	}
	marker := `{"validated":true,"echoed":"from-workspace"}`
	if err := os.WriteFile(filepath.Join(workspace, "marker.json"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	storeDir := t.TempDir()
	runner, err := NewEngineRunner(parentPath, iterlog.Nop())
	if err != nil {
		t.Fatalf("NewEngineRunner: %v", err)
	}
	defer func() { _ = runner.Close() }()
	runID, err := store.GenerateRunID()
	if err != nil {
		t.Fatalf("GenerateRunID: %v", err)
	}
	if derr := runner.Dispatch(context.Background(), DispatchSpec{
		RunID:         runID,
		WorkspacePath: workspace,
		StoreDir:      storeDir,
		Issue:         &IssueRef{ID: "native:" + runID, Identifier: runID, Title: "subbot worktree adoption"},
	}); derr != nil {
		t.Fatalf("Dispatch: %v", derr)
	}

	s, err := store.New(storeDir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ids, err := s.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	seen := false
	for _, id := range ids {
		if id == runID {
			continue
		}
		child, lerr := s.LoadRun(context.Background(), id)
		if lerr != nil || child.ParentRunID != runID {
			continue
		}
		seen = true
		if child.Worktree {
			t.Fatalf("child %s claimed its parent's workspace as a managed worktree "+
				"(RepoRoot=%q BaseCommit=%q) — finalization would commit and branch the tree "+
				"the parent is still writing in", id, child.RepoRoot, child.BaseCommit)
		}
		if child.RepoRoot != "" || child.BaseCommit != "" {
			t.Fatalf("child %s stamped a repo baseline it does not own: RepoRoot=%q BaseCommit=%q",
				id, child.RepoRoot, child.BaseCommit)
		}
	}
	if !seen {
		t.Fatal("no child run linked to the parent — the subbot node did not spawn one")
	}
}

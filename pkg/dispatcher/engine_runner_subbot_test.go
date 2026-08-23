package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
	"testing"

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
const slowSubbotChildBot = `
schema child_out:
  validated: bool
  echoed: string

tool work:
  command: "sleep 1.5; cat marker.json"
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

	// Catch the child mid-pass and prove its lock is held.
	childID := ""
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && childID == "" {
		ids, lerr := probe.ListRuns(context.Background())
		if lerr != nil {
			t.Fatalf("ListRuns: %v", lerr)
		}
		for _, id := range ids {
			if id == runID {
				continue
			}
			r, rerr := probe.LoadRun(context.Background(), id)
			if rerr != nil || r.ParentRunID != runID || r.Status != store.RunStatusRunning {
				continue
			}
			childID = id
			if lock, aerr := probe.LockRun(context.Background(), id); aerr == nil {
				_ = lock.Unlock()
				t.Fatalf("child %s is running but its run lock was free — the orphan reaper would read it as dead", id)
			}
			break
		}
		if childID == "" {
			time.Sleep(25 * time.Millisecond)
		}
	}
	if childID == "" {
		t.Fatal("never observed the child run in `running` state — cannot assert on its lock")
	}

	if derr := <-done; derr != nil {
		t.Fatalf("Dispatch: %v", derr)
	}
	lock, err := probe.LockRun(context.Background(), childID)
	if err != nil {
		t.Fatalf("child lock still held after the active pass — an external resume of a parked child would be blocked: %v", err)
	}
	_ = lock.Unlock()
}

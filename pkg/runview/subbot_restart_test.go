package runview

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestServiceLaunch_SubbotReattachAfterRestart is the regression for the
// reported bug: a parent parked on a child's human gate is an in-memory
// goroutine. When the process restarts, the parent is promoted to
// failed_resumable / cancelled while the CHILD stays answerable. Answering the
// child finishes it, but nothing picks it up; resuming the parent must
// RE-ATTACH to that answered child instead of spawning a fresh one (which would
// lose the child's work).
//
// The restart is simulated by cancelling the parent's launch context (its
// goroutine exits mid-park, exactly as a killed process would), then answering
// the orphaned child, then resuming the parent.
func TestServiceLaunch_SubbotReattachAfterRestart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gate_child.bot"), []byte(subbotGateChild), 0o644); err != nil {
		t.Fatalf("write child bot: %v", err)
	}
	parentPath := filepath.Join(dir, "gate_parent.bot")
	if err := os.WriteFile(parentPath, []byte(subbotGateParent), 0o644); err != nil {
		t.Fatalf("write parent bot: %v", err)
	}

	// The run gets a repository the test OWNS: without one, `worktree: auto`
	// (the IR default) takes os.Getwd() — this package inside the developer's
	// checkout — and registers the run's worktree there for good (#870).
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// Stop joins the orphan reconciler and every run goroutine before the
	// TempDir goes: a service left running writes into a directory RemoveAll
	// is already walking.
	t.Cleanup(func() { stopService(t, svc) })

	// 1. Launch the parent under a cancelable context (the "process").
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := svc.Launch(runCtx, LaunchSpec{FilePath: parentPath})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	parentID := res.RunID

	// 2. Wait for the child to park on its human gate AND the parent to have
	//    recorded it on the re-attach map (written before the child engine
	//    runs, so it is the durable state a restart depends on).
	childID := ""
	var lastChildStatus store.RunStatus
	var lastRecord map[string]string
	waitCtx := runWaitContext(t)
	for {
		if waitCtx.Err() != nil {
			// Name WHICH half is missing: "the child never got there" is a
			// slow machine, "the child parked but the key is absent" is the
			// product invariant this row exists for, and the old message
			// could not tell them apart.
			t.Fatalf("%v: child=%q status=%q, parent.SubbotChildren=%v — want a paused_waiting_human child recorded under run_child",
				waitCtx.Err(), childID, lastChildStatus, lastRecord)
		}
		runs, lerr := svc.ListRunRecordsCtx(context.Background(), ListFilter{})
		if lerr != nil {
			t.Fatalf("list runs: %v", lerr)
		}
		for _, r := range runs {
			if r.ParentRunID == parentID {
				lastChildStatus = r.Status
				if r.Status == store.RunStatusPausedWaitingHuman {
					childID = r.ID
				}
			}
		}
		p, _ := svc.store.LoadRun(context.Background(), parentID)
		if p != nil {
			lastRecord = p.SubbotChildren
		}
		if childID != "" && lastRecord["run_child"] == childID {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 3. Simulate the restart: cancel the parent's context so its parked
	//    goroutine exits. Wait for the run to settle to a resumable status.
	cancel()
	awaitRunCompletion(t, res.Done, "parent goroutine did not exit after context cancel")
	parent, err := svc.store.LoadRun(context.Background(), parentID)
	if err != nil {
		t.Fatalf("load parent after cancel: %v", err)
	}
	switch parent.Status {
	case store.RunStatusCancelled, store.RunStatusFailedResumable, store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator:
		// resumable — good
	default:
		t.Fatalf("parent status after restart = %q, want a resumable status", parent.Status)
	}
	// The re-attach record MUST survive the restart — that is the whole point.
	if parent.SubbotChildren["run_child"] != childID {
		t.Fatalf("re-attach record lost across restart: got %v want run_child=%s", parent.SubbotChildren, childID)
	}

	// 4. Answer the orphaned child the way the pipeline-board sidebar does.
	//    Nothing picks it up (the parent is down) — the child just finishes.
	child, err := svc.store.LoadRun(context.Background(), childID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	if _, err := svc.Resume(context.Background(), ResumeSpec{
		RunID:    childID,
		FilePath: child.FilePath,
		Answers:  map[string]any{"approved": true, "notes": "ship it"},
	}); err != nil {
		t.Fatalf("resume child: %v", err)
	}
	waitStatus(t, svc, childID, store.RunStatusFinished)

	// 5. Resume the parent. It must RE-ATTACH to the finished child (not spawn
	//    a fresh one) and finish with the child's terminal output.
	pres, err := svc.Resume(context.Background(), ResumeSpec{RunID: parentID, FilePath: parentPath})
	if err != nil {
		t.Fatalf("resume parent: %v", err)
	}
	awaitRunCompletion(t, pres.Done, "parent did not finish after resume")

	parent, err = svc.store.LoadRun(context.Background(), parentID)
	if err != nil {
		t.Fatalf("reload parent: %v", err)
	}
	if parent.Status != store.RunStatusFinished {
		t.Fatalf("parent status after resume = %q (error %q), want finished", parent.Status, parent.Error)
	}

	// The parent's downstream compute consumed {{outputs.run_child.verdict}} —
	// the RE-ATTACHED child's output, not a fresh child's.
	verdict := ""
	_ = svc.store.ScanEvents(context.Background(), parentID, func(e *store.Event) bool {
		if e.Type == store.EventNodeFinished && e.NodeID == "summarize" {
			if out, ok := e.Data["output"].(map[string]any); ok {
				verdict, _ = out["verdict"].(string)
			}
		}
		return true
	})
	if verdict != "ship it" {
		t.Errorf("summarize verdict = %q, want %q (re-attach lost the answered child's output)", verdict, "ship it")
	}

	// The decisive assertion: resume must NOT have spawned a second child for
	// run_child. Exactly one child run exists under this subbot node.
	childCount := 0
	runs, _ := svc.ListRunRecordsCtx(context.Background(), ListFilter{})
	for _, r := range runs {
		if r.ParentRunID == parentID && r.ParentNodeID == "run_child" {
			childCount++
		}
	}
	if childCount != 1 {
		t.Errorf("run_child child count = %d, want 1 (resume spawned a fresh child instead of re-attaching)", childCount)
	}

	// And the re-attach record is cleared once consumed.
	if _, ok := parent.SubbotChildren["run_child"]; ok {
		t.Errorf("re-attach record not cleared after the parent finished: %v", parent.SubbotChildren)
	}
}

func waitStatus(t *testing.T, svc *Service, runID string, want store.RunStatus) {
	t.Helper()
	ctx := runWaitContext(t)
	for ctx.Err() == nil {
		r, err := svc.store.LoadRun(context.Background(), runID)
		if err == nil && r.Status == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	r, _ := svc.store.LoadRun(context.Background(), runID)
	got := store.RunStatus("<load error>")
	if r != nil {
		got = r.Status
	}
	t.Fatalf("run %s status = %q, want %q", runID, got, want)
}

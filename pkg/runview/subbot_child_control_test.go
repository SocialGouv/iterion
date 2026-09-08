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

// Child: a tool node that blocks (sleep) long enough for us to act on the
// CHILD run while it is still executing its first pass. Parent: prep -> child.
const subbotControlChild = `
schema child_out:
  done: bool

tool spin:
  command: ` + "`sleep 30; printf '{\"done\":true}'`" + `
  output: child_out

workflow control_child:
  entry: spin
  spin -> done
`

// A looping child: each iteration is a ~1s node boundary, so the cooperative
// operator-pause signal (checked at the top of the exec loop, never mid-tool)
// is observed within a second instead of after a 30s blocking sleep.
const subbotControlLoopChild = `
schema tick_out:
  cont: bool

tool tick:
  command: ` + "`sleep 1; printf '{\"cont\":true}'`" + `
  output: tick_out

workflow control_child:
  entry: tick
  tick -> tick as spin(60) when cont
  tick -> done else
`

const subbotControlParent = `
schema pout:
  ready: bool

schema child_out:
  done: bool

tool prep:
  command: ` + "`printf '{\"ready\":true}'`" + `
  output: pout

subbot run_child:
  source: "control_child.bot"
  output: child_out

workflow control_parent:
  entry: prep
  prep -> run_child
  run_child -> done
`

// waitForChild polls the store until a child run of parentID exists in a
// non-terminal state, returning its id. Fails the test on timeout.
func waitForActiveChild(t *testing.T, svc *Service, parentID string) string {
	t.Helper()
	deadline := time.Now().Add(waitBudget(t, 30*time.Second))
	for {
		if time.Now().After(deadline) {
			t.Fatal("child run never appeared active")
		}
		runs, err := svc.ListRunRecordsCtx(context.Background(), ListFilter{})
		if err != nil {
			t.Fatalf("list runs: %v", err)
		}
		for _, r := range runs {
			if r.ParentRunID == parentID && r.Status == store.RunStatusRunning {
				return r.ID
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The regression for H2 (PR #193): a studio Cancel targeting the CHILD run id
// must act on it WHILE it runs its first pass, not silently no-op until the
// child pauses. Before the fix the in-process child engine ran unregistered,
// so manager.Cancel(childID) returned ErrRunNotActive.
func TestServiceLaunch_SubbotChild_CancelMidFlight(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "control_child.bot"), []byte(subbotControlChild), 0o644); err != nil {
		t.Fatalf("write child bot: %v", err)
	}
	parentPath := filepath.Join(dir, "control_parent.bot")
	if err := os.WriteFile(parentPath, []byte(subbotControlParent), 0o644); err != nil {
		t.Fatalf("write parent bot: %v", err)
	}

	// The run gets a repository the test OWNS: without one, `worktree: auto`
	// (the IR default) takes os.Getwd() — this package inside the developer's
	// checkout — and registers the run's worktree there for good (#870).
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: parentPath})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// A run goroutine that outlives the test writes into the store while
	// t.TempDir() is removing it — "directory not empty" on cleanup, a second
	// failure that hides the first.
	t.Cleanup(func() {
		_ = svc.Cancel(res.RunID)
		select {
		case <-res.Done:
		case <-time.After(waitBudget(t, 30*time.Second)):
			t.Error("the run goroutine outlived the test; its writes race t.TempDir() removal")
		}
	})

	childID := waitForActiveChild(t, svc, res.RunID)

	// The child must be individually controllable: it is registered with the
	// run Manager while executing, so Cancel(childID) reaches it (pre-fix this
	// returned ErrRunNotActive).
	if !svc.manager.Active(childID) {
		t.Fatalf("child %s is not registered with the manager mid-flight", childID)
	}
	if err := svc.Cancel(childID); err != nil {
		t.Fatalf("Cancel(child) = %v, want nil (child not controllable mid-flight)", err)
	}

	// The child ends cancelled, and the parent branch fails (its subbot node
	// returns the child's error) — not a hang.
	//
	// The ceiling is a HANG detector, and it must not be the child's own
	// blocking duration: the child sits in `sleep 30`, so a bare 30s here
	// reads "the tool node ran to completion" as "the parent hung", with a
	// margin equal to the launch→cancel gap alone. Measured post-cancel on a
	// starved box (GOMAXPROCS=1, 30 busy processes, n=30): 0.15-0.18s once the
	// run's source repository is one the test owns, 5.7-20.3s when it was the
	// developer's checkout — a spread that reached this ceiling on CI (#927).
	// waitBudget is the package's own allowance for a contended runner, which
	// every budget in the sibling subbot_restart_test.go already takes.
	//
	// Raising it does not make the row vacuous: the status assertion below
	// still refuses a child that merely ran its sleep out instead of being
	// cancelled.
	select {
	case <-res.Done:
	case <-time.After(waitBudget(t, 30*time.Second)):
		t.Fatal("parent did not terminate after the child was cancelled")
	}

	child, err := svc.store.LoadRun(context.Background(), childID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	if child.Status != store.RunStatusCancelled {
		t.Fatalf("child status = %q, want cancelled", child.Status)
	}

	// The manager handle is released once the child's pass returns, so a
	// later external resume can re-register the same id.
	if svc.manager.Active(childID) {
		t.Errorf("child %s still registered after it terminated — release missed", childID)
	}
}

// A studio Pause targeting the CHILD run id checkpoints it at the next safe
// boundary (paused_operator), leaving it resumable — the "ideally the operator
// pause signal" half of the fix. The child loops a short tool node so a pause
// boundary recurs every ~1s; the operator-paused child then PARKS the parent
// (like a human gate) until an external resume, so the parent stays running.
func TestServiceLaunch_SubbotChild_PauseMidFlight(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "control_child.bot"), []byte(subbotControlLoopChild), 0o644); err != nil {
		t.Fatalf("write child bot: %v", err)
	}
	parentPath := filepath.Join(dir, "control_parent.bot")
	if err := os.WriteFile(parentPath, []byte(subbotControlParent), 0o644); err != nil {
		t.Fatalf("write parent bot: %v", err)
	}

	// The run gets a repository the test OWNS: without one, `worktree: auto`
	// (the IR default) takes os.Getwd() — this package inside the developer's
	// checkout — and registers the run's worktree there for good (#870).
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: parentPath})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		_ = svc.Cancel(res.RunID)
		<-res.Done
	})

	childID := waitForActiveChild(t, svc, res.RunID)

	// Pause must be accepted by the manager (pre-fix: ErrRunNotActive).
	if err := svc.Pause(childID); err != nil {
		t.Fatalf("Pause(child) = %v, want nil", err)
	}

	// Within a few loop boundaries the child checkpoints as paused_operator.
	deadline := time.Now().Add(waitBudget(t, 30*time.Second))
	for {
		if time.Now().After(deadline) {
			child, _ := svc.store.LoadRun(context.Background(), childID)
			t.Fatalf("child never reached paused_operator (last status %q)", child.Status)
		}
		child, err := svc.store.LoadRun(context.Background(), childID)
		if err != nil {
			t.Fatalf("load child: %v", err)
		}
		if child.Status == store.RunStatusPausedOperator {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// An operator-paused child parks the parent's subbot node (awaiting an
	// external resume), exactly like a human gate — the parent stays running,
	// not failed.
	parent, err := svc.store.LoadRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	if parent.Status != store.RunStatusRunning {
		t.Fatalf("parent status while child paused = %q, want running", parent.Status)
	}
}

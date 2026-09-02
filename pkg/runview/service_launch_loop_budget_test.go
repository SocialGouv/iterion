package runview

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// loopBudgetBot is the campaign shape with no LLM in it: each pass is a
// shell node that reports its own token usage, the gate never converges,
// and a delivery tail sits on the fall-through the loop exits through.
// Three passes fit under the cap; the fourth does not.
const loopBudgetBot = `
schema pass_out:
  n: int

schema gate_out:
  converged: bool

schema deliver_out:
  delivered: bool

tool work:
  command: ` + "`printf '{\"n\":1,\"_tokens\":3000}'`" + `
  output: pass_out

compute gate:
  output: gate_out
  expr:
    converged: "false"

tool deliver:
  command: ` + "`printf '{\"delivered\":true}'`" + `
  output: deliver_out

workflow loop_budget_demo:
  ## The nodes run printf; the run needs neither a git checkout of the repo
  ## this test lives in (worktree defaults to auto) nor its toolchain
  ## (repo_devbox defaults to on). Both are pure latency here, and latency
  ## is what turns a bounded wait into a flake.
  worktree: none
  repo_devbox: off
  budget:
    max_tokens: 10000
  entry: work
  work -> gate
  gate -> deliver when converged
  gate -> work as passes(6)
  gate -> deliver
  deliver -> done
`

func writeLoopBudgetBot(t *testing.T, dir string) string {
	t.Helper()
	botPath := filepath.Join(dir, "loop_budget_demo.bot")
	if err := os.WriteFile(botPath, []byte(loopBudgetBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	return botPath
}

// launchLoopBudgetRun runs the bot once with the given run-level override
// and reports the terminal status and whether the delivery tail executed.
func launchLoopBudgetRun(t *testing.T, guard string) (store.RunStatus, bool) {
	t.Helper()
	dir := t.TempDir()
	botPath := writeLoopBudgetBot(t, dir)

	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	res, err := svc.Launch(context.Background(), LaunchSpec{
		FilePath:        botPath,
		LoopBudgetGuard: guard,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	select {
	case <-res.Done:
	case <-time.After(60 * time.Second):
		t.Fatal("run goroutine did not exit")
	}

	r, err := svc.store.LoadRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	events, err := svc.store.LoadEvents(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	delivered := false
	for _, ev := range events {
		if ev.NodeID == "deliver" && ev.Type == store.EventNodeFinished {
			delivered = true
		}
	}
	return r.Status, delivered
}

// TestLaunch_LoopBudgetGuardOverrideReachesTheEngine is the plumbing test
// for a knob whose whole purpose is to not be re-decided downstream. The
// same bot, launched twice through the same Service, must behave
// differently on the strength of LaunchSpec.LoopBudgetGuard alone —
// otherwise the field is decoration and the workflow silently wins.
func TestLaunch_LoopBudgetGuardOverrideReachesTheEngine(t *testing.T) {
	t.Setenv("ITERION_LOOP_BUDGET_GUARD", "")

	t.Run("unset: guarded, exits through the delivery tail", func(t *testing.T) {
		status, delivered := launchLoopBudgetRun(t, "")
		if status != store.RunStatusFinished {
			t.Errorf("status = %q, want %q", status, store.RunStatusFinished)
		}
		if !delivered {
			t.Error("delivery tail did not run — the banked work was stranded")
		}
	})

	t.Run("off: runs at the cap and strands the tail", func(t *testing.T) {
		status, delivered := launchLoopBudgetRun(t, "off")
		if status != store.RunStatusFailedResumable {
			t.Errorf("status = %q, want %q — the override did not reach the engine", status, store.RunStatusFailedResumable)
		}
		if delivered {
			t.Error("delivery tail ran with the guard off, want it stranded (that is what off means)")
		}
	})
}

package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/clock"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// threeStepWorkflow is a linear agent→agent→agent→done graph used to
// exercise the daily-cap pause at a node boundary.
func threeStepWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "cost_cap_test",
		Entry: "step1",
		Nodes: map[string]ir.Node{
			"step1": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step1"}},
			"step2": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step2"}},
			"step3": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step3"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "step1", To: "step2"},
			{From: "step2", To: "step3"},
			{From: "step3", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

// TestDailyCapPausesRunOnCross drives a run whose first node spends past
// the daily cap; the run must pause (resumable, paused_operator) at the
// next node boundary with a run_paused event tagged cost_cap_daily.
func TestDailyCapPausesRunOnCross(t *testing.T) {
	wf := threeStepWorkflow()

	exec := newStubExecutor()
	// step1 spends $2 — over the $1 cap. step2 should never run because
	// the cap pre-check pauses the run first.
	exec.on("step1", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 2.0, "_tokens": 100}, nil
	})
	step2Ran := false
	exec.on("step2", func(_ map[string]any) (map[string]any, error) {
		step2Ran = true
		return map[string]any{"ok": true}, nil
	})

	s := tmpStore(t)
	guard := NewDailyCapGuard(store.AsSpendStore(s), clock.Default, DailyCapConfig{MaxCostPerDayUSD: 1.0})
	if guard == nil {
		t.Fatal("expected non-nil guard")
	}
	eng := New(wf, s, exec, WithDailyCap(guard))

	err := eng.Run(context.Background(), "run-cap-1", nil)
	if !errors.Is(err, ErrRunPausedOperator) {
		t.Fatalf("expected ErrRunPausedOperator (cost-cap pause), got: %v", err)
	}
	if step2Ran {
		t.Error("step2 must not run after the cap is crossed")
	}

	r, err := s.LoadRun(context.Background(), "run-cap-1")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusPausedOperator {
		t.Errorf("status = %s, want paused_operator", r.Status)
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "step2" {
		t.Errorf("checkpoint = %+v, want NodeID=step2", r.Checkpoint)
	}

	// run_paused event must carry the cost-cap sentinel reason.
	events, _ := s.LoadEvents(context.Background(), "run-cap-1")
	sawCapPause := false
	for _, evt := range events {
		if evt.Type == store.EventRunPaused {
			if reason, _ := evt.Data["reason"].(string); reason == CapReasonDaily {
				sawCapPause = true
			}
		}
	}
	if !sawCapPause {
		t.Errorf("expected run_paused event with reason=%s", CapReasonDaily)
	}

	// The ledger recorded the spend.
	ds, _ := store.AsSpendStore(s).LoadDailySpend(context.Background(), clock.DayKey(clock.Default.Now()))
	if ds.SpentUSD < 2.0 {
		t.Errorf("ledger SpentUSD = %v, want >= 2.0", ds.SpentUSD)
	}
}

// TestDailyCapCountsFanOutBranchSpend is the regression test for the
// "fan-out spend escapes the ledger" blocker: LLM spend incurred inside
// parallel fan_out_all branches must land in the daily ledger (under
// per-branch keys) and count toward the cap. Before the fix, branch
// usage was recorded only to rs.budget, so the cap never saw it.
func TestDailyCapCountsFanOutBranchSpend(t *testing.T) {
	wf := fanOutWorkflow(ir.AwaitWaitAll)

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"summary": "go", "_cost_usd": 0.0}, nil
	})
	// Each parallel branch spends $0.60 — together $1.20, over the $1 cap.
	exec.on("agent_a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 0.60}, nil
	})
	exec.on("agent_b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 0.60}, nil
	})
	finalizeRan := false
	exec.on("finalize", func(_ map[string]any) (map[string]any, error) {
		finalizeRan = true
		return map[string]any{"ok": true}, nil
	})

	s := tmpStore(t)
	guard := NewDailyCapGuard(store.AsSpendStore(s), clock.Default, DailyCapConfig{MaxCostPerDayUSD: 1.0})
	eng := New(wf, s, exec, WithDailyCap(guard))

	err := eng.Run(context.Background(), "run-fanout-cap", nil)
	if !errors.Is(err, ErrRunPausedOperator) {
		t.Fatalf("expected cost-cap pause after branch spend crosses cap, got: %v", err)
	}
	if finalizeRan {
		t.Error("finalize (convergence node) must not run once branch spend trips the cap")
	}

	// The ledger must reflect BOTH branches' spend (~$1.20), recorded
	// under distinct per-branch keys so concurrent branches don't clobber.
	ds, err := store.AsSpendStore(s).LoadDailySpend(context.Background(), clock.DayKey(clock.Default.Now()))
	if err != nil {
		t.Fatalf("LoadDailySpend: %v", err)
	}
	if ds.SpentUSD < 1.2-1e-9 {
		t.Errorf("ledger SpentUSD = %v, want >= 1.20 (both branches counted)", ds.SpentUSD)
	}
	branchKeys := 0
	for k := range ds.RunsContributed {
		if strings.HasPrefix(k, "run-fanout-cap#") {
			branchKeys++
		}
	}
	if branchKeys != 2 {
		t.Errorf("expected 2 per-branch ledger keys, got %d (%+v)", branchKeys, ds.RunsContributed)
	}
}

// TestDailyCapUnderLimitRunsToCompletion confirms the cap is inert when
// spend stays under the limit.
func TestDailyCapUnderLimitRunsToCompletion(t *testing.T) {
	wf := threeStepWorkflow()
	exec := newStubExecutor()
	for _, id := range []string{"step1", "step2", "step3"} {
		exec.on(id, func(_ map[string]any) (map[string]any, error) {
			return map[string]any{"ok": true, "_cost_usd": 0.10}, nil
		})
	}
	s := tmpStore(t)
	guard := NewDailyCapGuard(store.AsSpendStore(s), clock.Default, DailyCapConfig{MaxCostPerDayUSD: 100.0})
	eng := New(wf, s, exec, WithDailyCap(guard))

	if err := eng.Run(context.Background(), "run-cap-ok", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), "run-cap-ok")
	if r.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want finished", r.Status)
	}
}

// TestDailyCapOverrideAllowsResume verifies that granting a same-day
// override lets a capped run resume past the boundary it paused at.
func TestDailyCapOverrideAllowsResume(t *testing.T) {
	wf := threeStepWorkflow()
	exec := newStubExecutor()
	exec.on("step1", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 2.0}, nil
	})
	exec.on("step2", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 0.0}, nil
	})
	exec.on("step3", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 0.0}, nil
	})

	s := tmpStore(t)
	clk := clock.NewFakeClock(time.Date(2026, 5, 30, 9, 0, 0, 0, time.UTC))
	guard := NewDailyCapGuard(store.AsSpendStore(s), clk, DailyCapConfig{MaxCostPerDayUSD: 1.0})
	eng := New(wf, s, exec, WithDailyCap(guard))

	// First pass pauses at step2.
	if err := eng.Run(context.Background(), "run-cap-resume", nil); !errors.Is(err, ErrRunPausedOperator) {
		t.Fatalf("expected cost-cap pause, got: %v", err)
	}

	// Operator overrides the cap for the day.
	if _, err := guard.SetOverride(context.Background(), true, "operator", "approved"); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}

	// Resume should now run to completion (no further pause).
	resumeEng := New(wf, s, exec, WithDailyCap(guard))
	err := resumeEng.Resume(context.Background(), "run-cap-resume", nil)
	if err != nil {
		t.Fatalf("Resume after override: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), "run-cap-resume")
	if r.Status != store.RunStatusFinished {
		t.Errorf("status after resume = %s, want finished", r.Status)
	}
}

package e2e

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The at-run budget overrides (`iterion run --max-cost-usd / --max-tokens
// / --max-duration / --max-iterations / --max-parallel-branches`) are the
// mechanism behind the documented "budget exceeded → raise the cap +
// resume" recovery, and the only lever an operator has over what a bot
// may spend without editing it. ir.ApplyBudgetOverrides is unit-tested;
// what was untested is whether `iterion run` actually applies it.
//
// The fixture's declared budget is deliberately too small to finish, so
// the run's OUTCOME is the readout: silently drop the override and the
// raise case stops converging; ignore the DSL budget and the baseline
// stops failing.

// runFixture executes a testdata `.bot` through the real CLI entry point
// with the scenario stub as executor, and returns the engine error.
func runFixture(t *testing.T, storeDir, runID string, budget ir.BudgetOverrides) error {
	t.Helper()
	return cli.RunRun(context.Background(), cli.RunOptions{
		File:          filepath.Join("testdata", "budget_override_mini.bot"),
		StoreDir:      storeDir,
		RunID:         runID,
		Budget:        budget,
		Executor:      newScenarioExecutor(),
		NoInteractive: true,
		MergeInto:     "none",
	}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON})
}

func loadRun(t *testing.T, storeDir, runID string) *store.Run {
	t.Helper()
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run %s: %v", runID, err)
	}
	return r
}

func runHasEvent(t *testing.T, storeDir, runID string, typ store.EventType) bool {
	t.Helper()
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, e := range events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// TestRunBudgetOverrideCapsTheRun pins the whole contract in one place:
// the bot's own too-small budget stops the run, and only the CLI
// override lets it through — or tightens it on another dimension.
func TestRunBudgetOverrideCapsTheRun(t *testing.T) {
	// --- baseline: the declared budget is authoritative ---------------
	// 4 nodes × 10 tokens against max_tokens: 25.
	baselineStore := t.TempDir()
	err := runFixture(t, baselineStore, "budget-baseline", ir.BudgetOverrides{})
	if err == nil {
		t.Fatal("baseline run finished, want the declared max_tokens: 25 to stop it")
	}
	var re *runtime.RuntimeError
	if !errors.As(err, &re) || re.Code != runtime.ErrCodeBudgetExceeded {
		t.Fatalf("baseline error = %v, want a BUDGET_EXCEEDED RuntimeError", err)
	}
	if got := loadRun(t, baselineStore, "budget-baseline").Status; got != store.RunStatusFailedResumable {
		t.Errorf("baseline status = %s, want failed_resumable (a budget stop must stay resumable)", got)
	}
	if !runHasEvent(t, baselineStore, "budget-baseline", store.EventBudgetExceeded) {
		t.Error("baseline run emitted no budget_exceeded event")
	}

	// --- raise the cap: the same bot now converges --------------------
	// This is the documented recovery. If the override never reached the
	// workflow, this run would fail exactly like the baseline.
	raisedStore := t.TempDir()
	if err := runFixture(t, raisedStore, "budget-raised", ir.BudgetOverrides{MaxTokens: 10_000}); err != nil {
		t.Fatalf("run with --max-tokens 10000: %v (the override did not reach the workflow)", err)
	}
	if got := loadRun(t, raisedStore, "budget-raised").Status; got != store.RunStatusFinished {
		t.Fatalf("raised-cap status = %s, want finished", got)
	}
	if runHasEvent(t, raisedStore, "budget-raised", store.EventBudgetExceeded) {
		t.Error("raised-cap run still emitted budget_exceeded")
	}

	// --- tighten another dimension: cost stops it even with tokens raised
	// Proves the override is applied per dimension, not as an all-or-nothing
	// swap of the declared budget.
	cappedStore := t.TempDir()
	err = runFixture(t, cappedStore, "budget-cost-capped", ir.BudgetOverrides{
		MaxTokens:  10_000,
		MaxCostUSD: 0.0015, // 4 nodes × $0.001
	})
	if err == nil {
		t.Fatal("run with --max-cost-usd 0.0015 finished, want the cost cap to stop it")
	}
	if !errors.As(err, &re) || re.Code != runtime.ErrCodeBudgetExceeded {
		t.Fatalf("cost-capped error = %v, want a BUDGET_EXCEEDED RuntimeError", err)
	}
	if !runHasEvent(t, cappedStore, "budget-cost-capped", store.EventBudgetExceeded) {
		t.Error("cost-capped run emitted no budget_exceeded event")
	}
}

package e2e

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A phase-budget guard is the reason the `run.*` namespace exists (#738):
// before it, a bot had to self-measure wall-clock in a tool node and mirror
// the `budget:` block through hand-maintained vars that drift from it in
// silence. This exercises the whole path — compile, execute, evaluate,
// route — through the real CLI entry point with the scenario stub, so the
// readout is which tail the run took, not a unit-tested accessor.
func TestRunBudgetNamespaceDrivesACompute(t *testing.T) {
	storeDir := t.TempDir()
	const runID = "run-budget-ns"

	err := cli.RunRun(context.Background(), cli.RunOptions{
		File:          filepath.Join("testdata", "run_budget_namespace_mini.bot"),
		StoreDir:      storeDir,
		RunID:         runID,
		Executor:      newScenarioExecutor(),
		NoInteractive: true,
		MergeInto:     "none",
	}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	run := loadRun(t, storeDir, runID)
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s (%s), want finished", run.Status, run.Error)
	}

	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	gauge := nodeOutput(t, events, "budget_gate")

	// The caps come from the `budget:` block through the namespace, not
	// from a var the bot maintains by hand.
	if got := gauge["cap"]; got != 0.0015 {
		t.Errorf("run.max_cost_usd = %v (%T), want 0.0015", got, got)
	}
	if got := toFloat(gauge["token_cap"]); got != 500 {
		t.Errorf("run.max_tokens = %v, want 500", gauge["token_cap"])
	}
	if got := toFloat(gauge["duration_cap"]); got != 1800 {
		t.Errorf("run.max_duration_seconds = %v, want 1800", gauge["duration_cap"])
	}

	// Consumption is the run's own, measured by the engine: the stub bills
	// the agent node $0.001 / 10 tokens.
	if got := gauge["spent"]; got != 0.001 {
		t.Errorf("run.cost_usd = %v (%T), want 0.001", got, got)
	}
	if got := toFloat(gauge["tokens"]); got != 10 {
		t.Errorf("run.tokens = %v, want 10", gauge["tokens"])
	}
	if got := toFloat(gauge["elapsed"]); got < 0 {
		t.Errorf("run.elapsed_seconds = %v, want >= 0", gauge["elapsed"])
	}

	// $0.001 of a $0.0015 cap is over half, so the guard must route to
	// `tight`. A nil on either side of the comparison silently picks the
	// `else` arm instead — which is exactly the failure a bot author would
	// never see.
	if gauge["over_budget"] != true {
		t.Fatalf("over_budget = %v, want true", gauge["over_budget"])
	}
	if nodeOutput(t, events, "tight") == nil {
		t.Error("the over-budget tail did not run")
	}
	if nodeOutput(t, events, "roomy") != nil {
		t.Error("the under-budget tail ran; the guard read a nil budget")
	}

	// `_duration_ms` rides next to `_cost_usd` on every executed node.
	burn := nodeOutput(t, events, "burn")
	if _, ok := burn["_duration_ms"]; !ok {
		t.Errorf("burn output carries no _duration_ms: %v", burn)
	}
}

// nodeOutput returns the output a node published on its node_finished
// event, or nil when the node never ran.
func nodeOutput(t *testing.T, events []*store.Event, nodeID string) map[string]any {
	t.Helper()
	for _, e := range events {
		if e.Type != store.EventNodeFinished || e.NodeID != nodeID {
			continue
		}
		out, _ := e.Data["output"].(map[string]any)
		return out
	}
	return nil
}

// toFloat normalises the JSON round trip an artifact goes through, where
// every number comes back as float64.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return -1
}

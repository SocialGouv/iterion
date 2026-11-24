package cli

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestALocalRunTellsTheExecutorItsRowsAreTheLaunchs pins the CLI half of the
// effort's provenance: a local launch resolves no routing policy, so every
// override row is the launch's own, and the executor is told so — a node
// effort `--effort-for` set reads `launch`, not the run's overrides.
//
// Mutation that reddens it: drop LaunchModelOverrides from buildRunExecutor's
// ExecutorSpec.
func TestALocalRunTellsTheExecutorItsRowsAreTheLaunchs(t *testing.T) {
	exec := buildExecutorForTest(t, RunOptions{EffortFor: []string{"review=low"}}, tiersMatchTheEngine, &ir.Workflow{Name: "wf", Nodes: map[string]ir.Node{}})
	claw, ok := exec.(*model.ClawExecutor)
	if !ok {
		t.Fatalf("the CLI built %T", exec)
	}
	rows, known := claw.LaunchModelOverrides()
	if !known {
		t.Fatal("the executor was not told which rows the launch carried")
	}
	if got := rows.ForNode("review", ir.NodeAgent).Effort; got != "low" {
		t.Errorf("the launch rows give review %q, want low", got)
	}
}

package runview

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestBuildRunnerCmdCarriesUnlimitedWorkflowOnRunAndResume(t *testing.T) {
	for _, command := range []runnerCommand{runnerCommandRun, runnerCommandResume} {
		cmd, err := buildRunnerCmd(context.Background(), "/bin/true", detachedSpec{
			Command: command, RunID: "run-1", FilePath: "wf.bot",
			Budget: &ir.BudgetOverrides{UnlimitedWorkflow: true, MaxParallelBranches: 1},
		})
		if err != nil {
			t.Fatalf("buildRunnerCmd(%s): %v", command, err)
		}
		joined := strings.Join(cmd.Args, " ")
		if !strings.Contains(joined, "--unlimited-workflow-budget") || !strings.Contains(joined, "--max-parallel-branches 1") {
			t.Fatalf("%s args = %q", command, joined)
		}
	}
}

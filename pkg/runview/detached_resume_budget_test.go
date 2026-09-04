package runview

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// E2 part 2: the detached-resume subprocess CLI must receive the
// resume-time budget flags. buildRunnerCmd's runnerCommandResume case
// did not emit any of the --max-* flags, so a detached resume ran the
// child on the .bot's launch-time cap — silently ignoring the operator's
// raise-the-cap ask. Same shape as the launch case that has always
// emitted them.
func TestBuildRunnerCmd_ResumeEmitsBudgetFlags(t *testing.T) {
	cmd, err := buildRunnerCmd(context.Background(), "iterion", detachedSpec{
		Command:  runnerCommandResume,
		RunID:    "run-1",
		FilePath: "/tmp/wf.bot",
		StoreDir: "/tmp/store",
		Budget: &ir.BudgetOverrides{
			MaxCostUSD:          120,
			MaxTokens:           5_000_000,
			MaxDuration:         "4h",
			MaxIterations:       50,
			MaxParallelBranches: 8,
		},
	})
	if err != nil {
		t.Fatalf("buildRunnerCmd: %v", err)
	}
	args := strings.Join(cmd.Args, " ")
	for flag, want := range map[string]string{
		"--max-cost-usd":          "120",
		"--max-tokens":            "5000000",
		"--max-duration":          "4h",
		"--max-iterations":        "50",
		"--max-parallel-branches": "8",
	} {
		if !strings.Contains(args, flag+" "+want) {
			t.Errorf("detached-resume args missing %q — the child would ignore the operator's raise-the-cap ask (E2)\nfull: %s", flag+" "+want, args)
		}
	}
}

// A resume spec with no budget must NOT emit any budget flag: an empty
// ask stays byte-identical to the pre-fix wire, and a zero-valued flag
// on the child (`--max-cost-usd 0`) would erase the .bot's cap.
func TestBuildRunnerCmd_ResumeWithoutBudgetEmitsNoBudgetFlags(t *testing.T) {
	cmd, err := buildRunnerCmd(context.Background(), "iterion", detachedSpec{
		Command:  runnerCommandResume,
		RunID:    "run-2",
		FilePath: "/tmp/wf.bot",
		StoreDir: "/tmp/store",
	})
	if err != nil {
		t.Fatalf("buildRunnerCmd: %v", err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, flag := range []string{
		"--max-cost-usd", "--max-tokens", "--max-duration",
		"--max-iterations", "--max-parallel-branches",
	} {
		if strings.Contains(args, flag) {
			t.Errorf("empty-budget resume emitted %q — a zero flag would erase the .bot's cap in the child\nfull: %s", flag, args)
		}
	}
}

package operatormcp

import (
	"strings"
	"testing"
)

func TestBuildRunnerArgsRun(t *testing.T) {
	args, err := buildRunnerArgs(runnerSpec{
		Command:             runnerCommandRun,
		RunID:               "rid",
		FilePath:            "/w/main.bot",
		StoreDir:            "/s/.iterion",
		Vars:                map[string]string{"b": "2", "a": "1"},
		Timeout:             "30m",
		MergeInto:           "none",
		BranchName:          "iterion/run/x",
		MaxCostUSD:          1.5,
		MaxTokens:           1000,
		MaxDuration:         "1h",
		MaxIterations:       3,
		MaxParallelBranches: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	want := "run /w/main.bot --background --run-id rid --no-interactive" +
		" --var a=1 --var b=2 --timeout 30m --merge-into none --branch-name iterion/run/x" +
		" --max-cost-usd 1.5 --max-tokens 1000 --max-duration 1h --max-iterations 3 --max-parallel-branches 2" +
		" --store-dir /s/.iterion"
	if got != want {
		t.Fatalf("argv mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildRunnerArgsResume(t *testing.T) {
	args, err := buildRunnerArgs(runnerSpec{
		Command:  runnerCommandResume,
		RunID:    "rid",
		FilePath: "/w/main.bot",
		StoreDir: "/s/.iterion",
		Force:    true,
		Answers:  map[string]string{"q2": "yes", "q1": "no"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	want := "resume --background --no-interactive --run-id rid --file /w/main.bot" +
		" --force --answer q1=no --answer q2=yes --store-dir /s/.iterion"
	if got != want {
		t.Fatalf("argv mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildRunnerArgsUnknownCommand(t *testing.T) {
	if _, err := buildRunnerArgs(runnerSpec{Command: "nope"}); err == nil {
		t.Fatal("unknown command should error")
	}
}

package operatormcp

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
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

// TestReapRunnerComparesPid pins the compare-and-delete contract: a
// reaped child may only remove the .pid that still records ITS OWN
// pid — a sibling that overwrote the file (spawn race) keeps its
// liveness record when the loser exits.
func TestReapRunnerComparesPid(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pidS := store.AsPIDStore(st)
	if pidS == nil {
		t.Fatal("filesystem store must implement PIDStore")
	}
	if _, err := st.CreateRun(context.Background(), "run-reap", "wf", nil); err != nil {
		t.Fatal(err)
	}

	child := exec.Command("true")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}

	// .pid holds ANOTHER runner's pid → the reap must leave it alone.
	if err := pidS.WritePIDFile("run-reap", child.Process.Pid+1); err != nil {
		t.Fatal(err)
	}
	reapRunner(child, pidS, "run-reap")
	if pid, err := pidS.ReadPIDFile("run-reap"); err != nil || pid != child.Process.Pid+1 {
		t.Fatalf(".pid of a surviving sibling was touched: pid=%d err=%v", pid, err)
	}

	// .pid holds THIS child's pid → the reap removes it.
	child2 := exec.Command("true")
	if err := child2.Start(); err != nil {
		t.Fatal(err)
	}
	if err := pidS.WritePIDFile("run-reap", child2.Process.Pid); err != nil {
		t.Fatal(err)
	}
	reapRunner(child2, pidS, "run-reap")
	if pid, err := pidS.ReadPIDFile("run-reap"); err != nil || pid != 0 {
		t.Fatalf("own .pid should be removed after reap: pid=%d err=%v", pid, err)
	}
}

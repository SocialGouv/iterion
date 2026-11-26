package ciguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// test-unit is sharded (#3402): the committed partition step is executed with
// a fake Go, so what is observed is what the shell actually passes, including
// whether a failed assignment stops it before `go test` runs on nothing.
func TestUnitShardsRunEveryIndexAndPropagateSelectionFailure(t *testing.T) {
	job := unitJob(t)
	indices := job.Strategy.Matrix["shard"]
	if len(indices) < 2 || len(job.Strategy.Matrix) != 1 || job.Strategy.FailFast == nil || *job.Strategy.FailFast {
		t.Fatal("test-unit needs multiple shards on one complete matrix axis with fail-fast disabled")
	}
	seen := map[int]bool{}
	for _, index := range indices {
		if index < 0 || index >= len(indices) || seen[index] {
			t.Fatalf("matrix %v does not cover every index 0..%d exactly once", indices, len(indices)-1)
		}
		seen[index] = true
	}
	var step string
	for _, candidate := range job.Steps {
		if strings.Contains(candidate.Run, "./scripts/ci/testshards") {
			if step != "" {
				t.Fatal("more than one unit partition step")
			}
			step = candidate.Run
		}
	}
	if step == "" {
		t.Fatal("test-unit never selects a package partition")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	const chosen = "example.com/chosen/a example.com/chosen/b"
	for _, index := range indices {
		for _, selectionExit := range []int{0, 7} {
			t.Run(fmt.Sprintf("shard%d/selection%d", index, selectionExit), func(t *testing.T) {
				dir := t.TempDir()
				// The quarantine call answers on its own branch, so RUN_ARGS keeps
				// the partition's arguments (quarantine_gate_test.go owns the skip).
				goScript := `#!/bin/sh
case "$1" in
  run)
    case "$*" in *./scripts/ci/testquarantine*) printf '%s\n' '^TestQuarantined$'; exit 0 ;; esac
    printf '%s\n' "$@" > "$RUN_ARGS"
    printf '%s\n' "` + chosen + `"
    exit "$SELECTION_EXIT"
    ;;
  test)
    printf '%s\n' "$@" >> "$TEST_ARGS"
    ;;
  *) exit 93 ;;
esac
`
				if err := os.WriteFile(filepath.Join(dir, "go"), []byte(goScript), 0o755); err != nil {
					t.Fatal(err)
				}
				runArgs, testArgs := filepath.Join(dir, "run"), filepath.Join(dir, "test")
				t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin:/bin")
				t.Setenv("RUN_ARGS", runArgs)
				t.Setenv("TEST_ARGS", testArgs)
				t.Setenv("SELECTION_EXIT", strconv.Itoa(selectionExit))
				cmd := exec.Command(bash, "-c", strings.ReplaceAll(step, "${{ matrix.shard }}", strconv.Itoa(index)))
				output, execErr := cmd.CombinedOutput()
				if selectionExit != 0 {
					if execErr == nil {
						t.Fatalf("step succeeded after the partition failed: %s", output)
					}
					if _, err := os.Stat(testArgs); !os.IsNotExist(err) {
						t.Fatal("go test ran after the partition failed: it would test no package, or every one")
					}
					return
				}
				if execErr != nil {
					t.Fatalf("step: %v\n%s", execErr, output)
				}
				readArgs := func(path string) []string {
					t.Helper()
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					return strings.Split(strings.TrimSpace(string(data)), "\n")
				}
				flagValue := func(args []string, flag string) string {
					t.Helper()
					at := slices.Index(args, flag)
					if at < 0 || at+1 == len(args) {
						t.Fatalf("missing %s value in %v", flag, args)
					}
					return args[at+1]
				}
				selection, execution := readArgs(runArgs), readArgs(testArgs)
				if flagValue(selection, "-n") != strconv.Itoa(len(indices)) || flagValue(selection, "-i") != strconv.Itoa(index) {
					t.Fatalf("selection %v does not match matrix %v", selection, indices)
				}
				skip, err := regexp.Compile(flagValue(selection, "-skip"))
				if err != nil || !skip.MatchString("github.com/SocialGouv/iterion/e2e") || slices.ContainsFunc([]string{"pkg/e2ehelpers", "pkg/x/e2e", "studio/e2e"}, func(other string) bool { return skip.MatchString("github.com/SocialGouv/iterion/" + other) }) {
					t.Fatalf("the partition does not exclude exactly the e2e package, which test-e2e owns: %v", selection)
				}
				// native_exec_runner_test.go holds the Bubblewrap-driven
				// packages out of this partition and into test-native-exec.
				weights := flagValue(selection, "-weights")
				if _, err := os.Stat(filepath.Join("..", "..", weights)); err != nil {
					t.Fatalf("the unit weights table %s is not committed: %v", weights, err)
				}
				pkgs := strings.Fields(chosen)
				if !slices.Equal(execution[len(execution)-len(pkgs):], pkgs) {
					t.Fatalf("go test did not receive the selected packages: %v", execution)
				}
				if !slices.Contains(execution, "-count=1") || flagValue(execution, "-timeout") != "1500s" {
					t.Fatalf("execution reused cached results or lost its package budget: %v", execution)
				}
			})
		}
	}
}

// A one-time step pinned to a shard the matrix does not have runs on no shard
// at all — vet, gofmt or the freshness gate would vanish from every run, green.
func TestUnitOneTimeStepsRunOnAShardThatExists(t *testing.T) {
	job := unitJob(t)
	indices := job.Strategy.Matrix["shard"]
	pinned := regexp.MustCompile(`matrix\.shard == (\d+)`)
	oneTime := map[string]bool{
		"Generated artefacts are fresh (fail fast)": false,
		"gofmt (check)":             false,
		"go vet":                    false,
		"Test capability census":    false,
		"Publish test capabilities": false,
		"Live e2e compile check":    false,
	}
	for _, step := range job.Steps {
		m := pinned.FindAllStringSubmatch(step.If, -1)
		if len(m) == 0 {
			continue
		}
		if len(m) > 1 {
			t.Errorf("step %q pins more than one shard: %q", step.Name, step.If)
			continue
		}
		n, _ := strconv.Atoi(m[0][1])
		if !slices.Contains(indices, n) {
			t.Errorf("step %q runs on shard %d, which the matrix %v does not have: it runs nowhere", step.Name, n, indices)
		}
		if _, ok := oneTime[step.Name]; ok {
			oneTime[step.Name] = true
		}
	}
	for name, found := range oneTime {
		if !found {
			t.Errorf("one-time step %q is not pinned to one shard: it is missing, or runs on every shard", name)
		}
	}
}

// unitLegOverheadMinutes is what a test-unit leg spends BEFORE its `Unit
// tests` step on the scale set, with margin: over the 335 successful ARC legs
// of 2026-11-25 the slowest reached the step after 10.85 minutes (run
// 45572758932, last shard, which also runs the freshness gate, gofmt, vet and
// the census). The steps after it (the live e2e compile check and the post
// steps, up to ~5.8 minutes) do not count: a hung step never reaches them.
// The bound is on the step's start, not each package's: `go test -timeout`
// counts from each test binary's own start, so a package whose binary starts
// later than the ceiling minus the timeout minus this setup can still be cut
// bare (run 45572758932, pkg/runview, ~4.4 minutes into the step).
const unitLegOverheadMinutes = 15

// unitQueueWaitMinutes is the pod wait a merge-group test-unit leg and then
// the `test` aggregator sit through before running, rounded up: on
// 2026-11-25, with the heavy layer out of the queue (ADR-173), the leg waited
// at most 13.9 minutes and the aggregator 3.8. With the heavy layer back the
// waits grow (2026-11-25 heavy-mode groups: leg p90 17.9 max 28.0, aggregator
// p90 4.0 max 11.3), so ending that mode forces this number, and the ceiling,
// to be measured again: TestUnitLegCeilingLeavesThePackageTimeoutRoom fails
// once race-shard runs in the merge queue.
const unitQueueWaitMinutes = 18

// The leg's job ceiling must leave the `go test -timeout` its own room: a
// package that runs to that per-package bound panics with a goroutine dump
// naming the hung test, while a job cut by `timeout-minutes` reports a bare
// cancellation and turns `test` red with nothing to read. With setup on a
// loaded pool, a 30-minute ceiling cut legs whose every package was still
// within its 25 minutes (runs 45652256463 and 45663630605). The ceiling also
// stays under the heavy legs', since test-unit is the light layer's fast
// signal, and it leaves the merge queue's check timeout room for the pod
// waits: a hung leg reports through the `test` aggregator after its own pod
// wait, the ceiling and the aggregator's pod wait, and passing the queue's
// check_response_timeout_minutes ends the group as checks_timed_out, which
// dequeues every entry behind it. The rule covers test-unit only;
// test-native-exec, whose plain and race runs sit back to back under a
// 30-minute ceiling, does not meet it. The floor holds for a package that
// starts with the step; one whose binary starts later in the step has less
// room, so the rule keeps the step's start inside the bound, not every
// package's.
func TestUnitLegCeilingLeavesThePackageTimeoutRoom(t *testing.T) {
	job := unitJob(t)
	var step string
	for _, candidate := range job.Steps {
		if strings.Contains(candidate.Run, "./scripts/ci/testshards") {
			step = candidate.Run
		}
	}
	timeout, err := time.ParseDuration(goTestTimeout(t, "test-unit", step))
	if err != nil {
		t.Fatalf("test-unit's go test -timeout is not a duration: %v", err)
	}
	floor := int(timeout.Minutes()) + unitLegOverheadMinutes
	if job.TimeoutMinutes < floor {
		t.Errorf("test-unit's job ceiling is %d minutes, under its go test -timeout (%s) plus the %d minutes a leg spends outside that step on the scale set: the job is cancelled before a hung package can panic and name itself", job.TimeoutMinutes, timeout, unitLegOverheadMinutes)
	}
	var wf struct {
		Jobs map[string]struct {
			TimeoutMinutes int    `yaml:"timeout-minutes"`
			If             string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatal(err)
	}
	race, ok := wf.Jobs["race-shard"]
	if !ok || race.TimeoutMinutes == 0 {
		t.Fatal("tests.yml has no race-shard job with a timeout-minutes")
	}
	if !strings.Contains(race.If, "github.event_name != 'merge_group'") {
		t.Errorf("race-shard runs in the merge queue again (ADR-173's light mode ended): the heavy legs' pod waits apply, so measure unitQueueWaitMinutes (%d) and test-unit's ceiling again against the queue timeout", unitQueueWaitMinutes)
	}
	if job.TimeoutMinutes >= race.TimeoutMinutes {
		t.Errorf("test-unit's job ceiling (%d minutes) reaches race-shard's (%d): the light layer's fast signal must stay under the heavy legs' bound", job.TimeoutMinutes, race.TimeoutMinutes)
	}
	if queue := queueCheckTimeoutMinutes(t); job.TimeoutMinutes+unitQueueWaitMinutes >= queue {
		t.Errorf("test-unit's job ceiling (%d minutes) plus the %d minutes of leg and aggregator pod waits reaches the merge queue's %d-minute check timeout: a hung leg ends its group as checks_timed_out and dequeues every entry behind it; decide the ceiling again against the waits measured now", job.TimeoutMinutes, unitQueueWaitMinutes, queue)
	}
}

type unitJobShape struct {
	TimeoutMinutes int `yaml:"timeout-minutes"`
	Strategy       struct {
		FailFast *bool            `yaml:"fail-fast"`
		Matrix   map[string][]int `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []struct {
		Name string `yaml:"name"`
		If   string `yaml:"if"`
		Run  string `yaml:"run"`
	} `yaml:"steps"`
}

func unitJob(t *testing.T) unitJobShape {
	t.Helper()
	var wf struct {
		Jobs map[string]yaml.Node `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatal(err)
	}
	var job unitJobShape
	node, ok := wf.Jobs["test-unit"]
	if !ok {
		t.Fatal("workflow has no test-unit job")
	}
	if err := node.Decode(&job); err != nil {
		t.Fatal(err)
	}
	return job
}

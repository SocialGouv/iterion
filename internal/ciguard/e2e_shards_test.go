package ciguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Execute the committed step with a fake Go: this observes the arguments
// the shell actually passes, including whether a failed assignment stops it.
func TestE2EShardsRunEveryIndexAndPropagateDiscoveryFailure(t *testing.T) {
	var wf struct {
		Jobs map[string]yaml.Node `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatal(err)
	}
	var job struct {
		Strategy struct {
			FailFast *bool            `yaml:"fail-fast"`
			Matrix   map[string][]int `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []struct {
			Run string `yaml:"run"`
		} `yaml:"steps"`
	}
	node := wf.Jobs["test-e2e"]
	if err := node.Decode(&job); err != nil {
		t.Fatal(err)
	}
	indices := job.Strategy.Matrix["shard"]
	if len(indices) < 2 || len(job.Strategy.Matrix) != 1 || job.Strategy.FailFast == nil || *job.Strategy.FailFast {
		t.Fatal("e2e needs multiple shards on one complete matrix axis with fail-fast disabled")
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
				t.Fatal("more than one e2e partition step")
			}
			step = candidate.Run
		}
	}
	if step == "" {
		t.Fatal("e2e never selects a test partition")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range indices {
		for _, discoveryExit := range []int{0, 7} {
			t.Run(fmt.Sprintf("shard%d/discovery%d", index, discoveryExit), func(t *testing.T) {
				dir := t.TempDir()
				// The quarantine call answers on its own branch, so RUN_ARGS keeps
				// the discovery's arguments (quarantine_gate_test.go owns the skip).
				goScript := `#!/bin/sh
case "$1" in
  run)
    case "$*" in *./scripts/ci/testquarantine*) printf '%s\n' '^TestQuarantined$'; exit 0 ;; esac
    printf '%s\n' "$@" > "$RUN_ARGS"
    printf '%s\n' "$GOFLAGS" > "$RUN_GOFLAGS"
    printf '%s\n' '^(TestChosen|Example|FuzzChosen)$'
    exit "$DISCOVERY_EXIT"
    ;;
  test)
    printf '%s\n' "$@" >> "$TEST_ARGS"
    ;;
  *) exit 93 ;;
esac
`
				if err := os.WriteFile(filepath.Join(dir, "go"), []byte(goScript), 0755); err != nil {
					t.Fatal(err)
				}
				runArgs, testArgs := filepath.Join(dir, "run"), filepath.Join(dir, "test")
				t.Setenv("PATH", dir)
				t.Setenv("RUN_ARGS", runArgs)
				t.Setenv("RUN_GOFLAGS", filepath.Join(dir, "goflags"))
				t.Setenv("GOFLAGS", "")
				t.Setenv("TEST_ARGS", testArgs)
				t.Setenv("DISCOVERY_EXIT", strconv.Itoa(discoveryExit))
				cmd := exec.Command(bash, "-c", strings.ReplaceAll(step, "${{ matrix.shard }}", strconv.Itoa(index)))
				output, execErr := cmd.CombinedOutput()
				if discoveryExit != 0 {
					if execErr == nil {
						t.Fatalf("step succeeded after discovery failed: %s", output)
					}
					if _, err := os.Stat(testArgs); !os.IsNotExist(err) {
						t.Fatal("go test ran after failed discovery emitted a partial pattern")
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
				if flagValue(selection, "-tests") != execution[len(execution)-1] {
					t.Fatalf("discovery and execution target different packages: %v / %v", selection, execution)
				}
				if flagValue(execution, "-run") != "^(TestChosen|Example|FuzzChosen)$" || !slices.Contains(execution, "-count=1") {
					t.Fatalf("execution lost the selected partition or reused cached results: %v", execution)
				}
				if !slices.Contains(execution, "-race") {
					t.Fatalf("e2e runs without the race detector, and race-shard leaves it out: e2e would never run under -race (#3664): %v", execution)
				}
				// Discovery builds the variant execution runs: a file tagged
				// `//go:build race` is listed only under -race.
				if goflags := readArgs(filepath.Join(dir, "goflags")); !slices.Contains(strings.Fields(strings.Join(goflags, " ")), "-race") {
					t.Fatalf("discovery runs with GOFLAGS=%q while execution runs -race: a race-tagged e2e test would never be discovered", strings.Join(goflags, " "))
				}
			})
		}
	}
}

// e2e runs under -race ONCE, in test-e2e on the scale set (#3664): the leg
// builds with cgo (the detector needs it), routes like every other ARC job,
// and race-shard leaves e2e out of its partition so the package's heaviest
// run is not paid twice. The execution test above holds the -race flag itself.
func TestE2ERunsUnderRaceOnceOnTheScaleSet(t *testing.T) {
	var wf struct {
		Jobs map[string]nativeJobShape `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatal(err)
	}
	e2e, race := wf.Jobs["test-e2e"], wf.Jobs["race-shard"]
	if got := e2e.Env["CGO_ENABLED"]; got != "1" {
		t.Errorf("test-e2e sets CGO_ENABLED=%q: -race needs cgo", got)
	}
	if !strings.HasSuffix(e2e.RunsOn, "|| 'arc-runners' }}") || e2e.RunsOn != wf.Jobs["golangci"].RunsOn {
		t.Errorf("test-e2e no longer runs on the scale set (runs-on %q)", e2e.RunsOn)
	}
	var partition string
	for _, step := range race.Steps {
		if strings.Contains(step.Run, "./scripts/ci/testshards") {
			partition = step.Run
		}
	}
	skip := splitterSkip(t, "race-shard", partition)
	if !skip.MatchString("github.com/SocialGouv/iterion/e2e") {
		t.Errorf("race-shard still partitions e2e (-skip %q): the package would run under -race twice", skip)
	}
	// test-e2e runs ./e2e alone: another package whose last element is e2e
	// must stay in the race partition, or it runs under -race nowhere.
	for _, other := range []string{"pkg/e2ehelpers", "pkg/x/e2e", "studio/e2e"} {
		if skip.MatchString("github.com/SocialGouv/iterion/" + other) {
			t.Errorf("race-shard's -skip %q drops %s, which test-e2e does not run", skip, other)
		}
	}
	// The -race timings measured so far are race-shard's own, whole packages
	// on hosted VMs; hash-split e2e shards are uneven and no ARC -race shard
	// wall is measured yet, so the shard keeps race-shard's proven bounds.
	if e2e.TimeoutMinutes != race.TimeoutMinutes {
		t.Errorf("test-e2e's job bound is %d minutes, race-shard's %d: the e2e -race shard keeps race-shard's proven bound", e2e.TimeoutMinutes, race.TimeoutMinutes)
	}
	var e2eRun string
	for _, step := range e2e.Steps {
		if strings.Contains(step.Run, "./scripts/ci/testshards") {
			e2eRun = step.Run
		}
	}
	if got, want := goTestTimeout(t, "test-e2e", e2eRun), goTestTimeout(t, "race-shard", partition); got != want {
		t.Errorf("test-e2e's go test -timeout is %s, race-shard's %s: the e2e -race shard keeps race-shard's proven per-package bound", got, want)
	}
}

// goTestTimeout reads the -timeout value of the one `go test` line of a step.
func goTestTimeout(t *testing.T, job, run string) string {
	t.Helper()
	var values []string
	for _, line := range strings.Split(run, "\n") {
		if !strings.Contains(line, "go test ") {
			continue
		}
		fields := strings.Fields(line)
		at := slices.Index(fields, "-timeout")
		if at < 0 || at+1 == len(fields) {
			t.Fatalf("%s: go test carries no -timeout: %s", job, strings.TrimSpace(line))
		}
		values = append(values, fields[at+1])
	}
	if len(values) != 1 {
		t.Fatalf("%s: want one go test line, got %d", job, len(values))
	}
	return values[0]
}

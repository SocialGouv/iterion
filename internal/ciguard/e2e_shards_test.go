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
				goScript := `#!/bin/sh
case "$1" in
  run)
    printf '%s\n' "$@" > "$RUN_ARGS"
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
			})
		}
	}
}

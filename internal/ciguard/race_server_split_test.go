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

// raceSplitPackage is the package race-shard divides by test name rather
// than packing whole (#3684): alone it ran ~1434 s under -race on ARC, the
// largest package after bots, which still runs whole.
const raceSplitPackage = "pkg/server"

// TestRaceShardsSplitPkgServerByTestName executes race-shard's partition step
// with a fake Go for every matrix index, so what is observed is what the
// shell passes:
//
//   - the package partition leaves pkg/server out (and only it: its
//     sub-packages stay packed), so no shard runs it whole;
//   - each shard discovers pkg/server's top-level tests under -race with
//     `-tests ./pkg/server -n <matrix width> -i <index>` and runs exactly that
//     pattern against ./pkg/server under -race — the matrix covers every index
//     once, and scripts/ci/testshards' TestTestPatternsCoverEveryGoInventory
//     NameOnce proves the patterns of indices 0..n-1 select every name once,
//     so every pkg/server test runs in exactly one shard;
//   - a discovery failure stops the step before any `go test` runs, and a red
//     pkg/server slice or a red package run each turn the shard red, although
//     the slice runs in the background beside the packages.
func TestRaceShardsSplitPkgServerByTestName(t *testing.T) {
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
	node := wf.Jobs["race-shard"]
	if err := node.Decode(&job); err != nil {
		t.Fatal(err)
	}
	indices := job.Strategy.Matrix["shard"]
	if len(indices) < 2 || len(job.Strategy.Matrix) != 1 || job.Strategy.FailFast == nil || *job.Strategy.FailFast {
		t.Fatal("race-shard needs multiple shards on one complete matrix axis with fail-fast disabled")
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
				t.Fatal("more than one race partition step")
			}
			step = candidate.Run
		}
	}
	if step == "" {
		t.Fatal("race-shard never selects a partition")
	}
	skip := splitterSkip(t, "race-shard", step)
	if !skip.MatchString("github.com/SocialGouv/iterion/" + raceSplitPackage) {
		t.Errorf("race-shard's package partition (-skip %q) still packs %s whole: its tests would run in one shard and again in their name slices", skip, raceSplitPackage)
	}
	for _, sub := range []string{"pkg/server/projects", "pkg/server/cloudpublisher", "pkg/serverx"} {
		if skip.MatchString("github.com/SocialGouv/iterion/" + sub) {
			t.Errorf("race-shard's -skip %q drops %s, which the name split does not run: it would run under -race nowhere", skip, sub)
		}
	}

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	const pattern = "^(TestChosenA|TestChosenB)$"
	// Every `go test` invocation lands in its own file: the slice and the
	// packages run concurrently, and one shared file would interleave them.
	const goScript = `#!/bin/sh
case "$1" in
  run)
    case "$*" in
      *./scripts/ci/testquarantine*) printf '%s\n' '^TestQuarantined$'; exit 0 ;;
      *" -tests "*)
        printf '%s\n' "$@" > "$DISCOVERY_ARGS"
        printf '%s\n' "$GOFLAGS" > "$DISCOVERY_GOFLAGS"
        printf '%s\n' '` + pattern + `'
        exit "$DISCOVERY_EXIT"
        ;;
      *) printf '%s\n' 'example.com/chosen/a example.com/chosen/b'; exit 0 ;;
    esac
    ;;
  test)
    f=$(mktemp "$TEST_DIR/inv.XXXXXX")
    printf '%s\n' "$@" > "$f"
    case "$*" in
      *" ./pkg/server"*) exit "$SERVER_EXIT" ;;
      *) exit "$PACKAGES_EXIT" ;;
    esac
    ;;
  *) exit 93 ;;
esac
`
	type scenario struct {
		name                                string
		discoveryExit, serverExit, pkgsExit int
		wantGreen                           bool
	}
	scenarios := []scenario{
		{"green", 0, 0, 0, true},
		{"discovery-fails", 7, 0, 0, false},
		{"server-slice-fails", 0, 1, 0, false},
		{"packages-fail", 0, 0, 1, false},
	}
	for _, index := range indices {
		for _, sc := range scenarios {
			t.Run(fmt.Sprintf("shard%d/%s", index, sc.name), func(t *testing.T) {
				dir := t.TempDir()
				bin, testDir := filepath.Join(dir, "bin"), filepath.Join(dir, "tests")
				for _, d := range []string{bin, testDir} {
					if err := os.Mkdir(d, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(bin, "go"), []byte(goScript), 0o755); err != nil {
					t.Fatal(err)
				}
				discoveryArgs, discoveryFlags := filepath.Join(dir, "discovery"), filepath.Join(dir, "goflags")
				cmd := exec.Command(bash, "--noprofile", "--norc", "-e", "-c", strings.ReplaceAll(step, "${{ matrix.shard }}", strconv.Itoa(index)))
				cmd.Dir = dir
				cmd.Env = []string{
					"PATH=" + bin + string(os.PathListSeparator) + "/usr/bin:/bin",
					"HOME=" + dir,
					"RUNNER_TEMP=" + dir,
					"GOFLAGS=",
					"TEST_DIR=" + testDir,
					"DISCOVERY_ARGS=" + discoveryArgs,
					"DISCOVERY_GOFLAGS=" + discoveryFlags,
					"DISCOVERY_EXIT=" + strconv.Itoa(sc.discoveryExit),
					"SERVER_EXIT=" + strconv.Itoa(sc.serverExit),
					"PACKAGES_EXIT=" + strconv.Itoa(sc.pkgsExit),
				}
				output, runErr := cmd.CombinedOutput()
				invocations, err := filepath.Glob(filepath.Join(testDir, "inv.*"))
				if err != nil {
					t.Fatal(err)
				}
				if sc.discoveryExit != 0 {
					if runErr == nil {
						t.Fatalf("the step succeeded after pkg/server discovery failed: %s", output)
					}
					if len(invocations) != 0 {
						t.Fatalf("go test ran %d time(s) after pkg/server discovery failed: a failed discovery must stop the shard before any test", len(invocations))
					}
					return
				}
				if (runErr == nil) != sc.wantGreen {
					t.Fatalf("step green=%v, want %v (%v)\n%s", runErr == nil, sc.wantGreen, runErr, output)
				}
				readLines := func(path string) []string {
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
				discovery := readLines(discoveryArgs)
				if flagValue(discovery, "-tests") != "./"+raceSplitPackage || flagValue(discovery, "-n") != strconv.Itoa(len(indices)) || flagValue(discovery, "-i") != strconv.Itoa(index) {
					t.Fatalf("discovery %v does not select shard %d of the %d-wide matrix over ./%s", discovery, index, len(indices), raceSplitPackage)
				}
				if goflags := readLines(discoveryFlags); !slices.Contains(strings.Fields(strings.Join(goflags, " ")), "-race") {
					t.Fatalf("discovery runs with GOFLAGS=%q while execution runs -race: a race-tagged test would never be discovered", strings.Join(goflags, " "))
				}
				if len(invocations) != 2 {
					t.Fatalf("the shard ran go test %d time(s), want 2 (the packages and the %s slice)", len(invocations), raceSplitPackage)
				}
				var server, packages []string
				for _, path := range invocations {
					args := readLines(path)
					if args[len(args)-1] == "./"+raceSplitPackage {
						server = args
					} else {
						packages = args
					}
				}
				if server == nil || packages == nil {
					t.Fatalf("want one go test over ./%s and one over the packages, got %v", raceSplitPackage, invocations)
				}
				if flagValue(server, "-run") != pattern {
					t.Fatalf("the %s run does not execute the discovered slice: %v", raceSplitPackage, server)
				}
				if slices.Contains(packages, "-run") || slices.ContainsFunc(packages, func(a string) bool { return strings.Contains(a, raceSplitPackage) }) {
					t.Fatalf("the package run narrows its tests or names %s: %v", raceSplitPackage, packages)
				}
				if !slices.Contains(packages, "example.com/chosen/a") || !slices.Contains(packages, "example.com/chosen/b") {
					t.Fatalf("the package run lost the partition's packages: %v", packages)
				}
				for _, args := range [][]string{server, packages} {
					if !slices.Contains(args, "-race") || !slices.Contains(args, "-count=1") {
						t.Fatalf("go test %v runs without -race or reuses cached results", args)
					}
				}
				if flagValue(server, "-timeout") != flagValue(packages, "-timeout") {
					t.Fatalf("the %s slice runs at -timeout %s, the packages at %s: one shard, one per-package bound", raceSplitPackage, flagValue(server, "-timeout"), flagValue(packages, "-timeout"))
				}
			})
		}
	}
}

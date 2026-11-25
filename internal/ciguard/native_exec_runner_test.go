package ciguard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// nativeExecJob is the one gated leg that runs on a namespace-capable VM
// instead of the scale set (#3662): ARC pods refuse the Bubblewrap read-only
// mount the native transport witnesses need.
const nativeExecJob = "test-native-exec"

// nativeExecPackages derives, from the test sources rather than from a list,
// the packages whose tests drive Bubblewrap — the ones ARC cannot run. A new
// such package lands in this set by being written, so the guards below hold
// it to the dedicated job without anyone remembering to.
func nativeExecPackages(t *testing.T) []string {
	t.Helper()
	const tree = "../.."
	seen := map[string]bool{}
	err := filepath.WalkDir(tree, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != tree && (strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_") || d.Name() == "vendor" || d.Name() == "testdata" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), bwrapLookup) {
			rel, err := filepath.Rel(tree, filepath.Dir(path))
			if err != nil {
				return err
			}
			seen[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree for Bubblewrap-driven tests: %v", err)
	}
	var pkgs []string
	for p := range seen {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	if len(pkgs) == 0 {
		t.Fatal("no test drives Bubblewrap any more: the dedicated native job proves nothing — fold it back into the shards and delete this guard in the same change")
	}
	return pkgs
}

// nativeExecRoutingTail is test-native-exec's last alternatives: its own
// variable, falling back to a GitHub-hosted VM, never a scale set.
const nativeExecRoutingTail = routingTailHead + "vars.CI_NATIVE_EXEC_RUNNER || 'ubuntu-latest' }}"

// bwrapLookup is how a test reaches for Bubblewrap. Spelled in two halves so
// this file does not count itself among the packages it looks for.
const bwrapLookup = "exec.LookPath(" + `"bwrap")`

type nativeJobShape struct {
	RunsOn         string `yaml:"runs-on"`
	TimeoutMinutes int    `yaml:"timeout-minutes"`
	Defaults       struct {
		Run struct {
			Shell string `yaml:"shell"`
		} `yaml:"run"`
	} `yaml:"defaults"`
	Env   map[string]string `yaml:"env"`
	Needs []string          `yaml:"needs"`
	Steps []struct {
		Name     string            `yaml:"name"`
		Shell    string            `yaml:"shell"`
		Run      string            `yaml:"run"`
		If       string            `yaml:"if"`
		Env      map[string]string `yaml:"env"`
		Continue bool              `yaml:"continue-on-error"`
	} `yaml:"steps"`
}

// The native job keeps the trust boundary of every other job — the fork,
// dependency-bot and global-off guards of the ARC expression, byte for byte,
// with only the destination swapped — proves the mount before it tests, and
// runs every Bubblewrap-driven package both plain and under -race. test-unit
// and race-shard are back on the scale set, carry no mount preflight (it
// fails on an ARC pod) and leave those packages out of their partitions.
func TestNativeExecRunnersPreserveTrustBoundaryAndProveMounts(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(workflowPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Defaults struct {
			Run struct {
				Shell string `yaml:"shell"`
			} `yaml:"run"`
		} `yaml:"defaults"`
		Jobs map[string]nativeJobShape `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	if wf.Defaults.Run.Shell != "" {
		t.Fatal("workflow default shell changes native preflight execution contract")
	}
	// The base is golangci's clauses alone, with whichever class tail it
	// carries removed: no scale-set variable of any class reaches the native
	// job's expression. TestSelfHostedRoutingExpressionsAgree holds every
	// class on that one base.
	base, ok := routingBase(wf.Jobs["golangci"].RunsOn)
	if !ok {
		t.Fatalf("golangci carries no canonical routing tail (runs-on %q): re-evaluate the native trust boundary", wf.Jobs["golangci"].RunsOn)
	}
	expected := base + nativeExecRoutingTail
	const preflight = "sudo apt-get update\nsudo apt-get install -y bubblewrap build-essential\nbwrap --version\nbwrap --die-with-parent --ro-bind / / --dev /dev --proc /proc /bin/true"
	pkgs := nativeExecPackages(t)

	job, ok := wf.Jobs[nativeExecJob]
	if !ok {
		t.Fatalf("missing native test job %s", nativeExecJob)
	}
	if job.Defaults.Run.Shell != "" {
		t.Errorf("%s default shell changes native preflight execution contract", nativeExecJob)
	}
	if job.RunsOn != expected {
		t.Errorf("%s can lose its native mount capability or bypass the shared fork/bot/off guards: runs-on %q, want %q", nativeExecJob, job.RunsOn, expected)
	}
	if namesSelfHostedRunner(job.RunsOn) {
		t.Errorf("%s names a scale set or a scale-set variable (runs-on %q): setting that variable would route its Bubblewrap mount to an ARC pod, which refuses it", nativeExecJob, job.RunsOn)
	}
	prerequisiteIndex, found := -1, 0
	plain, race := map[string]bool{}, map[string]bool{}
	firstTest := -1
	for index, step := range job.Steps {
		if step.Name == "Native exec environment prerequisites" {
			found++
			prerequisiteIndex = index
			if step.If != "" || step.Continue || step.Shell != "" {
				t.Errorf("%s can skip or forgive the mount prerequisite", nativeExecJob)
			}
			if strings.TrimSpace(step.Run) != preflight {
				t.Errorf("%s must execute the reviewed native prerequisite block unchanged", nativeExecJob)
			}
			continue
		}
		if !strings.Contains(step.Run, "go test ") {
			continue
		}
		if firstTest < 0 {
			firstTest = index
		}
		if step.If != "" || step.Continue {
			t.Errorf("%s step %q can be skipped or forgiven", nativeExecJob, step.Name)
		}
		cgo := job.Env["CGO_ENABLED"]
		if v, ok := step.Env["CGO_ENABLED"]; ok {
			cgo = v
		}
		// The quarantine's -skip is the only selection the job may carry:
		// test-unit and race-shard leave the whole package out, so a test
		// this step does not select runs in no leg at all.
		for _, sel := range goTestSelections(step.Run) {
			for _, name := range sel.narrowing {
				t.Errorf("%s step %q narrows the package with -%s: test-unit and race-shard leave it out, so the rest would run in no leg", nativeExecJob, step.Name, name)
			}
			if sel.skips != 1 {
				t.Errorf("%s step %q carries %d -skip flag(s), want exactly the quarantine's one: test-unit and race-shard leave the package out, so a test skipped here runs in no leg", nativeExecJob, step.Name, sel.skips)
			}
		}
		isRace := strings.Contains(step.Run, "go test -race ")
		if isRace && cgo != "1" {
			t.Errorf("%s step %q runs -race with CGO_ENABLED=%q: the detector needs cgo", nativeExecJob, step.Name, cgo)
		}
		for _, pkg := range pkgs {
			if strings.Contains(step.Run, " ./"+pkg+"/") {
				if isRace {
					race[pkg] = true
				} else {
					plain[pkg] = true
				}
			}
		}
	}
	if found != 1 || firstTest < 0 || prerequisiteIndex >= firstTest {
		t.Errorf("%s must have exactly one native mount preflight before its test steps", nativeExecJob)
	}
	// One job, two verdicts: the plain run is what `test` lost when test-unit
	// dropped the package, the race run what `race` lost from race-shard.
	for _, aggregator := range []string{"test", "race"} {
		if !slices.Contains(wf.Jobs[aggregator].Needs, nativeExecJob) {
			t.Errorf("required aggregator %q does not need %s: the package's %s run would merge on a verdict that never read it", aggregator, nativeExecJob, map[string]string{"test": "plain", "race": "-race"}[aggregator])
		}
	}
	for _, pkg := range pkgs {
		if !plain[pkg] {
			t.Errorf("%s never tests ./%s/ without -race: test-unit leaves it out, so `test` would lose it", nativeExecJob, pkg)
		}
		if !race[pkg] {
			t.Errorf("%s never tests ./%s/ under -race: race-shard leaves it out, so `race` would lose it", nativeExecJob, pkg)
		}
	}

	for _, id := range []string{"test-unit", "race-shard"} {
		leg, ok := wf.Jobs[id]
		if !ok {
			t.Fatal("missing sharded test job", id)
		}
		if routingClass(leg.RunsOn) == "" {
			t.Errorf("%s left the scale set (runs-on %q): its packages need no namespace, and a hosted queue is what #3662 removed", id, leg.RunsOn)
		}
		helmFound, helmIndex, testIndex := 0, -1, -1
		var partition string
		for index, step := range leg.Steps {
			if strings.Contains(step.Run, "./scripts/ci/testshards") {
				testIndex = index
				partition = step.Run
			}
			if step.Name == "Native exec environment prerequisites" || strings.Contains(step.Run, "bwrap ") {
				t.Errorf("%s runs the Bubblewrap preflight on the scale set, whose pods refuse that mount: the leg would be red before testing", id)
			}
			if step.Name == "Prepare installed Helm chart dependencies" {
				helmFound++
				helmIndex = index
				if step.If != "" || step.Continue || step.Shell != "" || strings.TrimSpace(step.Run) != "if command -v helm >/dev/null 2>&1; then\n  sh scripts/chart-check-deps.sh charts/iterion\nfi" {
					t.Errorf("%s can skip or forgive preparation of an installed Helm's dependencies", id)
				}
			}
		}
		if helmFound != 1 || testIndex < 0 || helmIndex >= testIndex {
			t.Errorf("%s must prepare installed Helm's dependencies once before testing", id)
		}
		skip := splitterSkip(t, id, partition)
		for _, pkg := range pkgs {
			if !skip.MatchString("github.com/SocialGouv/iterion/" + pkg) {
				t.Errorf("%s partitions %s onto the scale set, whose pods refuse its Bubblewrap mount; its -skip %q must exclude it", id, pkg, skip)
			}
		}
		for _, other := range []string{"pkg/sandbox", "pkg/sandbox/kubernetesx", "pkg/sandbox/kubernetes/sub"} {
			if skip.MatchString("github.com/SocialGouv/iterion/" + other) {
				t.Errorf("%s's -skip %q also drops %s, which no other leg runs", id, skip, other)
			}
		}
	}
}

var splitterSkipFlag = regexp.MustCompile(`-skip '([^']*)'`)

// splitterSkip reads the -skip regexp of the testshards call in a step.
// goTestSelection is what one `go test` command of a step selects: the
// narrowing flags it carries and how many -skip flags it holds.
type goTestSelection struct {
	narrowing []string
	skips     int
}

// goTestSelections reads every `go test` command of a step's script, a
// backslash-continued command as one line and a flag under its optional
// "test." prefix as the flag go test reads.
func goTestSelections(run string) []goTestSelection {
	var out []goTestSelection
	for _, line := range strings.Split(strings.ReplaceAll(run, "\\\n", " "), "\n") {
		if !strings.Contains(line, "go test ") {
			continue
		}
		var sel goTestSelection
		for _, field := range strings.Fields(line) {
			if !strings.HasPrefix(field, "-") {
				continue
			}
			name, _, _ := strings.Cut(strings.TrimLeft(field, "-"), "=")
			name = strings.TrimPrefix(name, "test.")
			switch name {
			case "run", "list", "short", "bench", "fuzz", "failfast":
				sel.narrowing = append(sel.narrowing, name)
			case "skip":
				sel.skips++
			}
		}
		out = append(out, sel)
	}
	return out
}

func splitterSkip(t *testing.T, job, run string) *regexp.Regexp {
	t.Helper()
	for _, line := range strings.Split(run, "\n") {
		if !strings.Contains(line, "./scripts/ci/testshards") {
			continue
		}
		m := splitterSkipFlag.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("%s: the testshards call carries no -skip '<re>': %s", job, strings.TrimSpace(line))
		}
		re, err := regexp.Compile(m[1])
		if err != nil {
			t.Fatalf("%s: -skip %q: %v", job, m[1], err)
		}
		return re
	}
	t.Fatalf("%s: no testshards call", job)
	return nil
}

// go test honours every test flag under an optional "test." prefix, and a
// shell script continues a command across a trailing backslash: the native
// job's selection guard reads both spellings as the flag they carry.
func TestGoTestSelectionsReadPrefixedAndContinuedFlags(t *testing.T) {
	for _, tc := range []struct {
		name, run string
	}{
		{"test. prefix", "go test -count=1 -skip \"$Q\" -test.run '^TestOne$' ./pkg/sandbox/kubernetes/...\n"},
		{"continued line", "go test -count=1 -skip \"$Q\" \\\n  -run '^TestOne$' ./pkg/sandbox/kubernetes/...\n"},
		{"continued test. prefix", "go test -count=1 \\\n  --test.run='^TestOne$' -skip \"$Q\" ./pkg/sandbox/kubernetes/...\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sels := goTestSelections(tc.run)
			if len(sels) != 1 {
				t.Fatalf("read %d go test command(s), want 1: %+v", len(sels), sels)
			}
			if !slices.Equal(sels[0].narrowing, []string{"run"}) || sels[0].skips != 1 {
				t.Errorf("read narrowing %q and %d -skip, want [run] and 1", sels[0].narrowing, sels[0].skips)
			}
		})
	}
}

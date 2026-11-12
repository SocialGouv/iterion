package ciguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The local gate's definition is a FILE — .github/ci-gate-map.yaml — read by
// two consumers: scripts/ci/gate.sh enumerates the members, and this guard
// pins the map against the workflow and the Taskfile. Two literals in two
// places drift; one file with two readers cannot.
//
// Read from DISK, in this test process. doc.go states the rule and
// release_push_skip_test.go states the other half: `go test -overlay` hands a
// guard a file the walk never sees and comes back falsely green, a mistake
// already made once in this repository's history.

const gateMapPath = "../../.github/ci-gate-map.yaml"

type gateJob struct {
	Required    string   `yaml:"required"`
	Checks      int      `yaml:"checks"`
	Disposition string   `yaml:"disposition"`
	Members     []string `yaml:"members"`
	Aggregates  []string `yaml:"aggregates"`
	Mutates     bool     `yaml:"mutates"`
	Reason      string   `yaml:"reason"`
	Residual    string   `yaml:"residual"`
	Equivalence string   `yaml:"equivalence"`
}

type gateMap struct {
	Jobs map[string]gateJob `yaml:"jobs"`
}

func readGateMap(t *testing.T) gateMap {
	t.Helper()
	var m gateMap
	if err := yaml.Unmarshal(mustRead(t, gateMapPath), &m); err != nil {
		t.Fatalf("parse %s: %v", gateMapPath, err)
	}
	if len(m.Jobs) == 0 {
		t.Fatalf("%s names no job — a gate map that is empty would pass every assertion below by vacuity", gateMapPath)
	}
	return m
}

// TestTheCIGateMapIsTotalAndHonest is the drift guard. Five properties, each
// the mirror of one way the map can stop being true.
func TestTheCIGateMapIsTotalAndHonest(t *testing.T) {
	t.Parallel()

	m := readGateMap(t)
	var parsed workflowJobs
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &parsed); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	wf := parsed.Jobs

	// 1. TOTALITY, both directions. A job added to the workflow with no row is
	// a gate that does not know about it; a row naming a job the workflow does
	// not define is a member nobody will ever need.
	for name := range wf {
		if _, ok := m.Jobs[name]; !ok {
			t.Errorf("the workflow defines job %q and the gate map has no row for it: the gate cannot know what it does not run", name)
		}
	}
	for name := range m.Jobs {
		if _, ok := wf[name]; !ok {
			t.Errorf("the gate map has a row for %q, which %s does not define", name, workflowPaths[0])
		}
	}
	if len(m.Jobs) != len(wf) {
		t.Errorf("the map has %d rows for %d jobs", len(m.Jobs), len(wf))
	}
	// Raise the floor: routing_test.go accepts len(wf) >= 5 for a file with 19
	// jobs, which would pass on a workflow someone truncated.
	if len(wf) < 15 {
		t.Fatalf("%s defines %d jobs: either the workflow was gutted or this guard is reading the wrong file",
			workflowPaths[0], len(wf))
	}

	// 2. THE CHECK-RUN COUNT. The ruleset keys on CONTEXTS, and three jobs fan
	// out, so a map that counted jobs would under-report what must be green.
	for name, job := range wf {
		// The fan-out IS the longest matrix axis: a job with no matrix is one
		// check run, and a job whose axis has four values is four contexts
		// whose NAMES carry the value (`desktop-vet-cross (darwin)`).
		want := 1
		for _, axis := range job.Strategy.Matrix {
			if len(axis) > want {
				want = len(axis)
			}
		}
		if got := m.Jobs[name].Checks; got != want {
			t.Errorf("job %q produces %d check run(s) and the map says %d", name, want, got)
		}
	}

	// 3. EVERY REQUIRED CONTEXT IS COVERED — with the aggregator clause
	// EARNED, not assumed. "Required but unexpressible, so not my problem"
	// would be a loophole wide enough for the whole gate: `test` and `race`
	// are required pure aggregators, so excluding them unconditionally would
	// let the gate call itself complete while test-unit had no member.
	for name, job := range m.Jobs {
		if job.Required != "yes" {
			continue
		}
		if len(job.Members) > 0 {
			continue
		}
		if len(job.Aggregates) == 0 {
			t.Errorf("required context %q has no member and aggregates nothing: the gate is incomplete and the map does not say so",
				name)
			continue
		}
		for _, leg := range job.Aggregates {
			legRow, ok := m.Jobs[leg]
			if !ok {
				t.Errorf("required aggregator %q names leg %q, which has no row", name, leg)
				continue
			}
			if len(legRow.Members) == 0 {
				t.Errorf("required aggregator %q is excused because it aggregates %q — and %q has no member either: the excuse is a hole",
					name, leg, leg)
			}
		}
	}

	// 4. EVERY MEMBER IS A TASK THAT EXISTS. A member nobody defined makes
	// `task <member>` fail at the runner with a message about the Taskfile,
	// which reads as a red gate rather than as a broken map.
	tasks := taskfileTaskNames(t)
	for name, job := range m.Jobs {
		for _, mem := range job.Members {
			if !tasks[mem] {
				t.Errorf("job %q names member %q, which Taskfile.yml does not define", name, mem)
			}
		}
	}

	// 5. EVERY ROW SAYS SOMETHING. A disposition this runner does not know, a
	// `local` with no equivalence argument, a `partial`/`unwritten` with no
	// residual, an `unexpressible` with no reason — each is a row that looks
	// like information and carries none.
	for name, job := range m.Jobs {
		switch job.Disposition {
		case "local":
			if len(job.Members) == 0 {
				t.Errorf("job %q is `local` with no member", name)
			}
			if strings.TrimSpace(job.Equivalence) == "" {
				t.Errorf("job %q claims `local` and argues no equivalence: same tool, same flags, same scope has to be SAID, because a near-neighbour is not the gate",
					name)
			}
		case "partial", "unwritten":
			if strings.TrimSpace(job.Residual) == "" {
				t.Errorf("job %q is %q and names no residual: the whole point of the label is what is missing", name, job.Disposition)
			}
		case "unexpressible":
			if strings.TrimSpace(job.Reason) == "" {
				t.Errorf("job %q is `unexpressible` and gives no reason", name)
			}
		default:
			t.Errorf("job %q carries disposition %q, which the runner does not know", name, job.Disposition)
		}
		switch job.Required {
		case "yes", "staged", "pending", "no":
		default:
			t.Errorf("job %q carries required=%q, which is none of yes/staged/pending/no", name, job.Required)
		}
	}
}

// TestTheRequiredSetTheMapKeysOnIsTheOneTheRulesetUses pins the map's own
// notion of "required" against this package's requiredChecks, which is the
// in-tree authority. A map that drifted from it would report a complete gate
// over a context the ruleset blocks on.
func TestTheRequiredSetTheMapKeysOnIsTheOneTheRulesetUses(t *testing.T) {
	t.Parallel()

	m := readGateMap(t)
	// requiredChecks is a SET: every key is a context the merge queue must
	// see, and its bool is always true — the staged-ahead-of-the-ruleset one
	// (`fmt-check`) is distinguished by that list's comment, not by the value.
	// So the cross-check is membership, and the map is free to say which of
	// `yes` and `staged` a context is.
	for name := range requiredChecks {
		row, ok := m.Jobs[name]
		if !ok {
			// A required context that is not a job of this workflow —
			// revi/review is the case — has no row by construction.
			continue
		}
		switch row.Required {
		case "yes", "staged":
		default:
			t.Errorf("requiredChecks holds %q and the gate map says required=%q: a context the queue blocks on "+
				"must be `yes` or `staged` here, or the gate reports a complete run over a context that can refuse a merge",
				name, row.Required)
		}
	}
	// And the other direction: a row claiming `yes` or `staged` that
	// requiredChecks does not hold would make the gate refuse to call itself
	// complete over a context nothing blocks on.
	for name, row := range m.Jobs {
		if row.Required != "yes" && row.Required != "staged" {
			continue
		}
		if !requiredChecks[name] {
			t.Errorf("the gate map says %q is required=%q and requiredChecks does not hold it", name, row.Required)
		}
	}
}

// TestTheGateRunnerReadsEveryMemberOwnExitCode executes the runner against a
// STUB `task`, because the property under test is the runner's behaviour and a
// Go re-reading of its rules would prove nothing about the shipped script.
//
// The pattern is e2e_shards_test.go's: a fake binary earlier on PATH, driven
// by a file the test writes.
func TestTheGateRunnerReadsEveryMemberOwnExitCode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		redMember string
		wantCode  int
	}{
		{"every member green", "", 0},
		// Both orders: a runner that stopped at the first failure would pass
		// the second case and fail the first, so one order is not a witness.
		{"the FIRST member red", "first", 1},
		{"the LAST member red", "last", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, code, matrix := runGateWithStubTask(t, tc.redMember)
			if code != tc.wantCode {
				t.Errorf("the runner exited %d, want %d\n%s", code, tc.wantCode, out)
			}
			// `ran N of N` is the count the runner compares against its own
			// plan: "two of three members ran" is answered by a number, never
			// by a chain of &&.
			if !strings.Contains(out, "ci:gate: ran ") {
				t.Errorf("the runner does not report how many members it ran:\n%s", out)
			}
			plan := strings.Count(matrix, "\n")
			if plan == 0 {
				t.Fatalf("the matrix is empty: no member's exit code was recorded\n%s", out)
			}
			if !strings.Contains(out, fmt.Sprintf("ran %d of %d", plan, plan)) {
				t.Errorf("the runner ran %d member(s) and did not say `ran %d of %d`:\n%s", plan, plan, plan, out)
			}
			// EVERY member has a row, including the ones after a red one: a
			// runner that stopped early would leave the rest unmeasured and
			// its count would differ from its plan.
			if tc.redMember == "first" && plan < 2 {
				t.Errorf("the first member was red and only %d row(s) were recorded: the runner stopped instead of continuing", plan)
			}
		})
	}
}

// runGateWithStubTask writes a fake `task` that fails for one member and runs
// the shipped gate script against a two-member map in a scratch repository.
func runGateWithStubTask(t *testing.T, red string) (output string, code int, matrix string) {
	t.Helper()

	root := t.TempDir()
	// A real repository, because the script asks git for its top level.
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "guard@example.test"},
		{"config", "user.name", "guard"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "seed"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "seed"}, {"commit", "-qm", "seed"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	mustMkdir(t, filepath.Join(root, ".github"))
	mustMkdir(t, filepath.Join(root, "scripts", "ci"))
	mustMkdir(t, filepath.Join(root, "stub"))

	// Two members, neither mutating, both covering the one required context —
	// so the only thing varying between cases is which member is red.
	gate := `jobs:
  alpha:
    required: yes
    checks: 1
    disposition: local
    members: ["first"]
    equivalence: the stub
  beta:
    required: no
    checks: 1
    disposition: local
    members: ["last"]
    equivalence: the stub
`
	mustWrite(t, filepath.Join(root, ".github", "ci-gate-map.yaml"), gate, 0o644)

	// The SHIPPED script, copied rather than re-implemented: the property is
	// its behaviour.
	body := mustRead(t, scriptUnderTest(t, "../../scripts/ci/gate.sh"))
	mustWrite(t, filepath.Join(root, "scripts", "ci", "gate.sh"), string(body), 0o755)

	// The stub `task`: green unless it is the member this case reddens.
	stub := "#!/bin/sh\n"
	if red != "" {
		stub += fmt.Sprintf("[ \"$1\" = %q ] && { echo 'stub: red'; exit 7; }\n", red)
	}
	stub += "echo \"stub ran $1\"\nexit 0\n"
	mustWrite(t, filepath.Join(root, "stub", "task"), stub, 0o755)

	evidence := filepath.Join(root, "evidence")
	cmd := exec.Command("bash", filepath.Join(root, "scripts", "ci", "gate.sh"))
	cmd.Dir = root
	cmd.Env = append(scriptEnv(t),
		"PATH="+filepath.Join(root, "stub")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ITERION_CI_GATE_EVIDENCE="+evidence,
	)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run the gate: %v\n%s", err, buf.String())
		}
		code = exit.ExitCode()
	}
	raw, err := os.ReadFile(filepath.Join(evidence, "matrix.tsv"))
	if err != nil {
		t.Fatalf("the runner wrote no matrix: %v\n%s", err, buf.String())
	}
	return buf.String(), code, string(raw)
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// taskfileTaskNames reads the Taskfile's own task names. Keyed on the parsed
// mapping rather than on a regexp over the text, so a task renamed in a way a
// pattern would still match cannot pass.
func taskfileTaskNames(t *testing.T) map[string]bool {
	t.Helper()
	var doc struct {
		Tasks map[string]yaml.Node `yaml:"tasks"`
	}
	if err := yaml.Unmarshal(mustRead(t, taskfilePath), &doc); err != nil {
		t.Fatalf("parse %s: %v", taskfilePath, err)
	}
	if len(doc.Tasks) < 20 {
		t.Fatalf("%s parsed to %d tasks: this guard is reading the wrong shape", taskfilePath, len(doc.Tasks))
	}
	names := make(map[string]bool, len(doc.Tasks))
	for k := range doc.Tasks {
		names[k] = true
	}
	return names
}

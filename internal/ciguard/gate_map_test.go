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

	"github.com/SocialGouv/iterion/internal/gittest"
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

// derivedCount reads the mongo member's own count out of its own line, so the
// assertion is about what the member says it did.
var derivedCount = regexp.MustCompile(`mongo-conformance: (\d+) invocation\(s\) derived from`)

type gateJob struct {
	Required    string   `yaml:"required"`
	Checks      int      `yaml:"checks"`
	Disposition string   `yaml:"disposition"`
	Members     []string `yaml:"members"`
	Inlined     []string `yaml:"inlined"`
	Aggregates  []string `yaml:"aggregates"`
	Mutates     bool     `yaml:"mutates"`
	Reason      string   `yaml:"reason"`
	Residual    string   `yaml:"residual"`
	Equivalence string   `yaml:"equivalence"`
}

type gateMap struct {
	Jobs            map[string]gateJob `yaml:"jobs"`
	RefusingMembers []string           `yaml:"refusing_members"`
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
	//
	// `yes` only, and the division is deliberate: a `staged` context with no
	// member is a LEGITIMATE state (a context this repository decided will
	// block merges, ahead of anyone writing the member), and the runner is
	// what reports it — `scripts/ci/gate.sh` counts `staged` in its coverage
	// query and answers exit 3, "green but INCOMPLETE". This guard judges
	// whether the map is HONEST, not whether the gate is complete; making it
	// refuse a staged row with no member would turn a staging decision into a
	// red test. Property 6 below still holds a staged row whose command is
	// already a task, which is the case that actually misled a reader.
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

	// 4. EVERY MEMBER IS A TASK THAT EXISTS, AND ONE THAT CANNOT GO GREEN
	// WITHOUT RUNNING. A member nobody defined makes `task <member>` fail at
	// the runner with a message about the Taskfile, which reads as a red gate
	// rather than as a broken map.
	//
	// The second half is the one measured the hard way: `ignore_error: true`
	// turns a failure into 0, and `status:`/`sources:` make go-task print
	// "Task … is up to date" and exit 0 without running the command at all.
	// The runner records that 0 and prints "green, and every required or
	// staged context is covered" — the exact false green this whole file is
	// about, reached by a legal one-line edit to a task rather than to the
	// map. Held for EVERY member, not only the refusing ones, because that is
	// where the green is claimed.
	tasks := taskfileTaskNames(t)
	shapes := taskfileShapes(t)
	for name, job := range m.Jobs {
		for _, mem := range job.Members {
			if !tasks[mem] {
				t.Errorf("job %q names member %q, which Taskfile.yml does not define", name, mem)
				continue
			}
			if shapes[mem].canSkip {
				t.Errorf("job %q names member %q, and that task carries `%s`: go-task can then answer 0 "+
					"without the command having run or having failed, and the gate would report that as green",
					name, mem, shapes[mem].skipKey)
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

	// 6. A JOB WHOSE COMMAND IS ALREADY A TASK CANNOT BE WITHOUT THAT MEMBER.
	// Derived, not listed: every step's `run:` is matched against every
	// one-command task, and a byte-identical pair MUST appear in the row's
	// members. A hand-kept table of pairs would need someone to notice the
	// pair, which is the very thing that failed — `fmt-check` read `unwritten`
	// over a residual describing another job's step while `task fmt:check` ran
	// its one command verbatim, and the gate answered exit 3 on the strength of
	// it. The dispositions are prose; this is arithmetic over the two files.
	//
	// Scoped to what the gate CLAIMS: the required and staged contexts, plus
	// the legs a required aggregator speaks for — the same reachable set
	// scripts/ci/gate.sh computes. An advisory row with a stale disposition
	// misinforms a reader; one of these changes the verdict.
	reached := reachedRequired(m)
	for _, pair := range identicalStepAndTask(t) {
		row, ok := m.Jobs[pair.Job]
		if !ok {
			continue // totality is property 1's to report
		}
		if !reached[pair.Job] {
			continue
		}
		found := false
		for _, mem := range row.Members {
			for _, cand := range pair.Tasks {
				if mem == cand {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("job %q runs `%s` and Taskfile task(s) %v are that command byte for byte, yet the row's members are %v: "+
				"a job whose command is already a task has a member, whatever the row claims",
				pair.Job, pair.Command, pair.Tasks, row.Members)
		}
	}

	// 7. THE DECLARED IDENTITIES ARE THE DERIVED ONES — both directions.
	// Property 6 only forces `identical ⇒ member`. The converse is where a row
	// rots quietly: when a CI step gains a flag, the pair DISAPPEARS, property
	// 6 falls silent, and the row goes on arguing "the job's command verbatim"
	// over a command that is no longer it — with the runner still answering
	// exit 0. Measured before this property existed: a `--new-from-rev` added
	// to the required `golangci` job's step left the whole package green.
	//
	// So `inlined:` declares the identities and this compares the two sets per
	// job. A new identity must be declared (the class that let `fmt-check`
	// read `unwritten` while `task fmt:check` ran its command), and a dead one
	// reddens here rather than in a reader's head.
	derived := make(map[string][][]string)
	for _, pair := range identicalStepAndTask(t) {
		derived[pair.Job] = append(derived[pair.Job], pair.Tasks)
	}
	for name, job := range m.Jobs {
		// Declared but no longer identical: either side drifted.
		for _, declared := range job.Inlined {
			if !slices.ContainsFunc(derived[name], func(cands []string) bool {
				return slices.Contains(cands, declared)
			}) {
				t.Errorf("job %q declares `inlined: %q` and no `run:` of that job is that task's command any more: "+
					"the identity died and the row still argues it — fix the row, or restore the command",
					name, declared)
			}
		}
		// Identical but undeclared: the map does not know about a task that
		// already runs one of this job's steps.
		for _, cands := range derived[name] {
			if !slices.ContainsFunc(cands, func(c string) bool { return slices.Contains(job.Inlined, c) }) {
				t.Errorf("job %q has a `run:` step that Taskfile task(s) %v match byte for byte, and the row's "+
					"`inlined:` is %v: declare it, so the day the step drifts this guard says so",
					name, cands, job.Inlined)
			}
		}
	}

	// 8. THE REFUSING MEMBERS ARE MEMBERS. `refusing_members` is what lets the
	// runner answer 2 ("this machine cannot judge") instead of 1 ("the tree is
	// red") for a member that refuses on a precondition. A name in that list
	// that no row runs would be a promise about a member nobody invokes.
	if len(m.RefusingMembers) == 0 {
		t.Errorf("%s declares no `refusing_members`: three members refuse with exit 2 by design, and "+
			"without the list the runner reads an unreachable service as a broken repository", gateMapPath)
	}
	// A refusing member must have ONE failure path, and that is structural
	// rather than stylistic: measured on go-task 3.48.0, `task -x` over two
	// failing parallel `deps:` returns whichever finished first — 7 7 7 2 7 2
	// over ten runs of the same tree. A member with `deps:` would make the
	// gate flip between "red" and "refused" at random, and a verdict that
	// depends on an order nothing guarantees is not a verdict.

	for _, mem := range m.RefusingMembers {
		if !tasks[mem] {
			t.Errorf("`refusing_members` holds %q, which Taskfile.yml does not define", mem)
		}
		if shape, ok := shapes[mem]; ok && !shape.oneFailurePath {
			t.Errorf("`refusing_members` holds %q, and %s (%d command(s), %d dep(s), delegates=%t): "+
				"the runner then cannot tell a refusal from a red",
				mem, shape.why(), shape.cmds, shape.deps, shape.delegates)
		}
		used := false
		for _, job := range m.Jobs {
			if slices.Contains(job.Members, mem) {
				used = true
			}
		}
		if !used {
			t.Errorf("`refusing_members` holds %q and no row names it as a member: the runner would never "+
				"meet it, so the entry is a claim about nothing", mem)
		}
	}
}

// reachedRequired is the set of jobs the gate's completeness claim covers: the
// required and staged rows, and the legs a required aggregator names. It
// mirrors the REACHED query in scripts/ci/gate.sh — one notion of "what the
// verdict is about", read by the runner and by this guard.
func reachedRequired(m gateMap) map[string]bool {
	reached := make(map[string]bool, len(m.Jobs))
	for name, job := range m.Jobs {
		if job.Required != "yes" && job.Required != "staged" {
			continue
		}
		reached[name] = true
		for _, leg := range job.Aggregates {
			reached[leg] = true
		}
	}
	return reached
}

// stepTaskPair is one `run:` step and EVERY one-command task holding that
// exact command. A slice, not a name, because the Taskfile has three commands
// carried by two tasks each today (`studio:test`/`sdk:ts:test`,
// `studio:deps`/`sdk:ts:install`, `cli:build:linux:amd64{,:fast}`), and a
// map from command to a single name would keep whichever Go's randomised
// range order wrote last — measured: 7 red, 5 green over 12 runs of the same
// tree. A test that fails at random is worse than absent. Two of those pairs
// differ by their `dir:`, so "byte-identical command" is not even "same
// scope": the verdict is "ANY candidate is the member", and the message names
// them all.
type stepTaskPair struct {
	Job     string
	Command string
	Tasks   []string
}

// identicalStepAndTask pairs every workflow step with the one-command Taskfile
// tasks holding the SAME command. Byte equality after trimming, so a step that
// gained a path or a flag stops matching and the pair disappears rather than
// quietly standing for something weaker.
func identicalStepAndTask(t *testing.T) []stepTaskPair {
	t.Helper()

	var wf workflowSteps
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	var tf taskfileTasks
	if err := yaml.Unmarshal(mustRead(t, taskfilePath), &tf); err != nil {
		t.Fatalf("parse %s: %v", taskfilePath, err)
	}
	// One command only. A multi-command task that happens to begin with a
	// step's command runs MORE than the step, which is a different claim.
	//
	// BOTH spellings: go-task accepts the singular `cmd:`, and a task written
	// that way would be invisible here — including an identity of exactly the
	// kind this property exists to catch. The shapes reader is where the two
	// spellings are counted; this is the same rule applied to the text.
	byCommand := make(map[string][]string, len(tf.Tasks))
	for name, shape := range taskfileShapes(t) {
		// `command` is empty unless the task holds exactly one command and it
		// is a scalar — a `- task: other` delegation is a mapping with no
		// scalar text, so it cannot collide with a step's shell line.
		if shape.cmds == 1 && shape.command != "" {
			byCommand[shape.command] = append(byCommand[shape.command], name)
		}
	}
	// Sorted, so neither the candidate list nor the failure message depends on
	// the order Go handed back.
	for cmd := range byCommand {
		slices.Sort(byCommand[cmd])
	}

	var pairs []stepTaskPair
	for job, def := range wf.Jobs {
		for _, step := range def.Steps {
			run := strings.TrimSpace(step.Run)
			if run == "" {
				continue
			}
			if cands, ok := byCommand[run]; ok {
				pairs = append(pairs, stepTaskPair{Job: job, Command: run, Tasks: cands})
			}
		}
	}
	// Sorted by job then command: property 7 reports per job, and a test whose
	// error order moves between runs is harder to read than it needs to be.
	slices.SortFunc(pairs, func(a, b stepTaskPair) int {
		if a.Job != b.Job {
			return strings.Compare(a.Job, b.Job)
		}
		return strings.Compare(a.Command, b.Command)
	})
	if len(pairs) < 2 {
		t.Fatalf("only %d workflow step(s) match a one-command task: this repository inlines at least `fmt:check` "+
			"and `docs:links:test`, so a smaller result means the two files were parsed into the wrong shape",
			len(pairs))
	}
	return pairs
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
		name     string
		bad      []string
		wantCode int
	}{
		{"every member green", nil, 0},
		// A member that spends 2 AND is declared in `refusing_members` cannot
		// run here — it says nothing about the tree. Reading it as 1, "a
		// member is red", is how a sandboxed session would read an
		// unreachable docker as a broken repository.
		{"a declared refusing member", []string{"refuse-declared"}, 2},
		// The other direction, and the one that matters more: a member that
		// spends 2 WITHOUT declaring the convention is red, because plenty of
		// tools spend 2 on a real failure. Promoting those would turn a red
		// into a shrug.
		{"an undeclared member spending 2", []string{"refuse-undeclared"}, 1},
		// PRECEDENCE: a red member outranks a refusal. Without this case the
		// two verdict blocks could be swapped and nothing would notice — a red
		// tree would then read "exit 2, no verdict", which is the one direction
		// that turns a red into a shrug. Measured: with the blocks swapped the
		// whole package stayed green before this case existed.
		{"a red member AND a declared refusing one", []string{"first", "refuse-declared"}, 1},
		// Both orders: a runner that stopped at the first failure would pass
		// the second case and fail the first, so one order is not a witness.
		{"the FIRST member red", []string{"first"}, 1},
		{"the LAST member red", []string{"last"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, code, matrix := runGateWithStubTask(t, tc.bad, "")
			if code != tc.wantCode {
				t.Errorf("the runner exited %d, want %d\n%s", code, tc.wantCode, out)
			}
			// The `partial` leg behind the required aggregator is NAMED, on
			// every path: that sentence is what stops a 0 from reading as "CI
			// can find nothing this did not run".
			if !strings.Contains(out, "leg") {
				t.Errorf("the runner never names the `partial` leg its own map carries:\n%s", out)
			}
			// The mutating member runs LAST, so no member after it compiles
			// against a tree it rewrote mid-run.
			if rows := strings.Split(strings.TrimSpace(matrix), "\n"); len(rows) == stubGateMembers &&
				!strings.HasPrefix(rows[len(rows)-1], "writes\t") {
				t.Errorf("the mutating member is not the last row of the matrix:\n%s", matrix)
			}
			// `ran N of N` is the count the runner compares against its own
			// plan: "two of three members ran" is answered by a number, never
			// by a chain of &&.
			if !strings.Contains(out, "ci:gate: ran ") {
				t.Errorf("the runner does not report how many members it ran:\n%s", out)
			}
			// The expectation is the MAP's member count, not the matrix's line
			// count: deriving it from the output the runner wrote would make
			// the assertion agree with whatever the runner did. EVERY member
			// gets a row, including the ones after a red one — a runner that
			// stopped early would leave the rest unmeasured.
			if got := strings.Count(matrix, "\n"); got != stubGateMembers {
				t.Errorf("the matrix holds %d row(s) for a %d-member plan: %q\n%s",
					got, stubGateMembers, matrix, out)
			}
			if !strings.Contains(out, fmt.Sprintf("ran %d of %d", stubGateMembers, stubGateMembers)) {
				t.Errorf("the runner did not say `ran %d of %d`:\n%s", stubGateMembers, stubGateMembers, out)
			}
		})
	}
}

// stubGateMembers is how many members stubGateMap declares. The runner's
// `ran N of N` is checked against THIS, never against the matrix it wrote.
const stubGateMembers = 5

// stubGateMap is the map the runner reads in the harness. It carries one of
// every shape the runner BRANCHES on, because a stub that carries only the
// easy ones leaves the branches untested while looking like a harness: a
// required pure AGGREGATOR excused by its leg, a `partial` leg (the caveat
// path), a `mutates: true` member (the ordering), and two members that spend
// 2 — `refuse-declared` listed in `refusing_members`, `refuse-undeclared`
// deliberately not, which is what makes the refusal cases a pair rather than
// one claim.
const stubGateMap = `refusing_members: ["refuse-declared"]
jobs:
  agg:
    required: yes
    checks: 1
    disposition: unexpressible
    reason: a pure aggregator, excused by its leg
    aggregates: [leg]
  leg:
    required: no
    checks: 1
    disposition: partial
    members: ["first", "refuse-declared", "refuse-undeclared"]
    residual: the stub covers part of it
  beta:
    required: no
    checks: 1
    disposition: local
    members: ["last"]
    equivalence: the stub
  mutator:
    required: no
    checks: 1
    disposition: local
    members: ["writes"]
    mutates: true
    equivalence: the stub
`

// runGateWithStubTask writes a fake `task` that misbehaves for the named
// members and runs the shipped gate script in a scratch repository. A LIST,
// not one name: the precedence between a red member and a refusing one is a
// rule of the runner, and a harness that can only make one member misbehave
// at a time cannot observe it. `gateMap` empty means stubGateMap; `args` are
// passed to the script. A missing matrix comes back as "" rather than
// fataling, because a refusal before the first member is a case this harness
// now covers.
func runGateWithStubTask(t *testing.T, bad []string, gateMap string, args ...string) (output string, code int, matrix string) {
	t.Helper()

	root := t.TempDir()
	// A real repository, because the script asks git for its top level — and
	// built through internal/gittest, never a bare exec.Command("git", …).
	// Since git 2.48 a writing command DETACHES `git maintenance run --auto`,
	// which keeps writing under .git/objects after the command returned and
	// races t.TempDir()'s removal. pkg/git's TestEveryTestGitCallerDisables-
	// AutoMaintenance is what caught this file doing it, in CI, after the
	// local run of three packages had not asked.
	gittest.Run(t, root, "init", "-q")
	gittest.Run(t, root, "config", "user.email", "guard@example.test")
	gittest.Run(t, root, "config", "user.name", "guard")
	mustWrite(t, filepath.Join(root, "seed"), "seed\n", 0o644)
	gittest.Run(t, root, "add", "seed")
	gittest.Run(t, root, "commit", "-qm", "seed")

	mustMkdir(t, filepath.Join(root, ".github"))
	mustMkdir(t, filepath.Join(root, "scripts", "ci"))
	mustMkdir(t, filepath.Join(root, "stub"))

	if gateMap == "" {
		gateMap = stubGateMap
	}
	mustWrite(t, filepath.Join(root, ".github", "ci-gate-map.yaml"), gateMap, 0o644)

	// The SHIPPED script, copied rather than re-implemented: the property is
	// its behaviour.
	body := mustRead(t, scriptUnderTest(t, "../../scripts/ci/gate.sh"))
	mustWrite(t, filepath.Join(root, "scripts", "ci", "gate.sh"), string(body), 0o755)

	// The stub `task`, carrying the terms of the REAL producer — this is where
	// a kinder stub hid a critical defect for two rounds. Measured on the
	// pinned go-task 3.48.0: a task whose command exits 2 makes `task` exit
	// **201**, and so does one that exits 7; only `-x/--exit-code` passes the
	// code through. A stub that spent the code directly made the runner look
	// like it read per-member codes while every real failure arrived as 201.
	// So: the member is the LAST argument (with `-x` it is no longer `$1`),
	// and the code is swallowed into 201 unless `-x` is present.
	stub := "#!/bin/sh\n" +
		"pass=0\nfor a in \"$@\"; do case \"$a\" in -x|--exit-code) pass=1 ;; esac; done\n" +
		"for a in \"$@\"; do m=\"$a\"; done\n"
	for _, m := range bad {
		rc := 7
		if strings.HasPrefix(m, "refuse-") {
			rc = 2
		}
		stub += fmt.Sprintf("[ \"$m\" = %q ] && { echo 'stub: %d'; [ \"$pass\" = 1 ] && exit %d; exit 201; }\n", m, rc, rc)
	}
	stub += "echo \"stub ran $m\"\nexit 0\n"
	mustWrite(t, filepath.Join(root, "stub", "task"), stub, 0o755)

	evidence := filepath.Join(root, "evidence")
	cmd := exec.Command("bash", append([]string{filepath.Join(root, "scripts", "ci", "gate.sh")}, args...)...)
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
	// A refusal that fires BEFORE the first member writes no matrix, and that
	// is the correct behaviour for the cases below — so its absence is
	// reported as "" and the caller asserts on it.
	raw, err := os.ReadFile(filepath.Join(evidence, "matrix.tsv"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read the matrix: %v\n%s", err, buf.String())
	}
	return buf.String(), code, string(raw)
}

// TestTheGateRunnerRefusesAMapItCannotTrust is the other half: before this,
// three shapes of broken map each produced `ran 0 of 0` followed by "green,
// and every required context has a member", exit 0 — one of them in complete
// silence. A `mapfile -t X < <(yq …)` hides yq's status from `set -e` and from
// `pipefail` alike, so an empty array reads as "nothing to report".
//
// The scalar-`aggregates` case is the quiet one and the reason this test
// exists: ONE missing pair of brackets, and the coverage query splats a
// string, yields nothing, and drops a required context with no member out of
// the uncovered set. One character between "INCOMPLETE, exit 3" and "green,
// exit 0".
func TestTheGateRunnerRefusesAMapItCannotTrust(t *testing.T) {
	t.Parallel()

	// A map whose aggregator names its leg as a STRING. Correctly bracketed,
	// this is an exit 3: `test` is required, has no member, and its leg has
	// none either.
	scalarAggregates := `jobs:
  test:
    required: yes
    checks: 1
    disposition: unexpressible
    reason: a pure aggregator
    aggregates: leg
  leg:
    required: no
    checks: 1
    disposition: unwritten
    residual: nobody wrote one
  beta:
    required: no
    checks: 1
    disposition: local
    members: ["last"]
    equivalence: the stub
`
	bracketed := strings.Replace(scalarAggregates, "aggregates: leg", "aggregates: [leg]", 1)

	// A map whose REQUIRED row names its member as a string. Bracketed, this
	// is a plain green run; scalar, the member silently leaves the plan while
	// the row still counts as covered.
	scalarMembers := `jobs:
  alpha:
    required: yes
    checks: 1
    disposition: local
    members: first
    equivalence: the stub
  beta:
    required: no
    checks: 1
    disposition: local
    members: ["last"]
    equivalence: the stub
`
	membersBracketed := strings.Replace(scalarMembers, "members: first", `members: ["first"]`, 1)
	emptyMembers := strings.Replace(scalarMembers, "members: first", "members: []", 1)
	emptyAggregates := strings.Replace(scalarAggregates, "aggregates: leg", "aggregates: []", 1)

	for _, tc := range []struct {
		name      string
		gateMap   string
		args      []string
		wantCode  int
		wantInOut string
		wantNoRun bool
	}{
		{"an empty map", "\n", nil, 2, "does not parse into a non-empty", true},
		{"a map whose top-level key is not jobs", "ci_jobs:\n  a:\n    required: yes\n", nil, 2, "does not parse into a non-empty", true},
		{"a map that is not YAML", "jobs:\n  a:\n   required: yes\n     checks: 1\n", nil, 2, "does not parse into a non-empty", true},
		{"a scalar aggregates", scalarAggregates, nil, 2, "is not a NON-EMPTY sequence", true},
		// The same map, bracketed: the runner must now REPORT the uncovered
		// required context instead of dropping it. Without this arm the case
		// above would pass over a runner that refuses every map.
		{"the same map, bracketed", bracketed, nil, 3, "INCOMPLETE", false},
		// `members:` is the same missing bracket, and worse: the plan query
		// splats it so the member vanishes, while the coverage query reads
		// `.members == null` as false and counts the row COVERED — a required
		// context whose member never ran, announced green.
		{"a scalar members", scalarMembers, nil, 2, "is not a NON-EMPTY sequence", true},
		{"the same map, members bracketed", membersBracketed, nil, 0, "green, and every required", false},
		// `members: []` is a SEQUENCE, so a type-only precondition accepts it
		// — and in yq an empty sequence is falsy, so `select(.members)` drops
		// the row from the plan while `[] == null` is false and the coverage
		// query counts it covered. Two characters from the same false green.
		{"an empty members sequence", emptyMembers, nil, 2, "is not a NON-EMPTY sequence", true},
		{"an empty aggregates sequence", emptyAggregates, nil, 2, "is not a NON-EMPTY sequence", true},
		// An argument this runner does not know used to fall through to a FULL
		// run, mutating member included.
		// `refusing_members` is the third list the queries splat, and a scalar
		// there empties it inside a process substitution: every member that
		// refuses would then be read as RED.
		{"a scalar refusing_members", "refusing_members: ci:gofmt\n" + stubGateMap[strings.Index(stubGateMap, "jobs:"):], nil, 2, "is not a non-empty sequence", true},
		// `false` and `[]` leave the list just as empty, and `// []` treats
		// `false` as absent — so the key is tested with `has`.
		{"a false refusing_members", "refusing_members: false\n" + stubGateMap[strings.Index(stubGateMap, "jobs:"):], nil, 2, "is not a non-empty sequence", true},
		{"an empty refusing_members", "refusing_members: []\n" + stubGateMap[strings.Index(stubGateMap, "jobs:"):], nil, 2, "is not a non-empty sequence", true},
		{"an unknown argument", "", []string{"--dry-run"}, 2, "unknown argument", true},
		{"two arguments", "", []string{"--list", "--list"}, 2, "at most one", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, code, matrix := runGateWithStubTask(t, nil, tc.gateMap, tc.args...)
			if code != tc.wantCode {
				t.Errorf("the runner exited %d, want %d\n%s", code, tc.wantCode, out)
			}
			if !strings.Contains(out, tc.wantInOut) {
				t.Errorf("the runner does not say %q — a refusal nobody can read is a silent one:\n%s", tc.wantInOut, out)
			}
			// The sentence that must NEVER appear over a map the runner could
			// not read: it is the one the landing lane reads as its first
			// proof.
			if tc.wantCode != 0 && strings.Contains(out, "green, and every required") {
				t.Errorf("the runner announced a green gate while exiting %d:\n%s", code, out)
			}
			if tc.wantNoRun && matrix != "" {
				t.Errorf("the runner ran %d member(s) after refusing: %q", strings.Count(matrix, "\n"), matrix)
			}
		})
	}
}

// TestTheRunnerNeverGreensOverAMemberItDidNotRun is the GUARANTEE, after
// three consecutive rounds found one more SHAPE of the same seam: a scalar
// `aggregates:`, then a scalar `members:`, then an empty sequence — each time
// the runner answered 0 and announced "every required or staged context is
// covered" over a plan that had silently lost a member. Enumerating shapes
// pays one shape at a time; this quantifies over them.
//
// The oracle is a SECOND reader with a different, deliberately generous
// notion of what the author meant: Go's YAML sees a scalar `members: first`
// as the one member `first`, which is what the operator wrote down. The
// invariant is then shape-agnostic — exit 0 implies the matrix holds exactly
// the members the map declares — so a future shape nobody has thought of
// cannot pass by being unanticipated. It must either run every member the
// author named, or refuse.
func TestTheRunnerNeverGreensOverAMemberItDidNotRun(t *testing.T) {
	t.Parallel()

	body := func(alpha string) string {
		return "jobs:\n  alpha:\n    required: yes\n    checks: 1\n    disposition: local\n" +
			alpha + "    equivalence: the stub\n" +
			"  beta:\n    required: no\n    checks: 1\n    disposition: local\n    members: [\"last\"]\n    equivalence: the stub\n"
	}
	for name, gateMap := range map[string]string{
		"a sequence":             body("    members: [\"first\"]\n"),
		"a scalar":               body("    members: first\n"),
		"an empty sequence":      body("    members: []\n"),
		"a null":                 body("    members:\n"),
		"a mapping":              body("    members:\n      first: yes\n"),
		"a number":               body("    members: 7\n"),
		"a nested sequence":      body("    members: [[\"first\"]]\n"),
		"a sequence with a null": body("    members: [\"first\", null]\n"),
		"no members key":         body(""),
		// A member two rows share: the shape `unique` exists for, and the one
		// an oracle that compared multisets reddened on.
		"a member two jobs share": body("    members: [\"last\"]\n"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out, code, matrix := runGateWithStubTask(t, nil, gateMap)
			declared := declaredMembers(t, gateMap)
			var ran []string
			for _, row := range strings.Split(strings.TrimSpace(matrix), "\n") {
				if row == "" {
					continue
				}
				ran = append(ran, strings.SplitN(row, "\t", 2)[0])
			}
			// COMPACTED, both sides: the runner deduplicates with `unique`
			// because two rows naming the same member is legitimate, and an
			// oracle comparing multisets would redden on a legal map — a
			// false red in a tool that blocks a landing costs what a hole
			// costs. Measured before this line existed: a map where two jobs
			// share `first` failed while the runner was right.
			slices.Sort(declared)
			slices.Sort(ran)
			declared = slices.Compact(declared)
			ran = slices.Compact(ran)
			// A map this runner cannot use is a REFUSAL (2), or an honest
			// INCOMPLETE (3) — never 1, "a member is red", which is a claim
			// about the tree. Without this, `exit 2` could be written `exit 1`
			// in any precondition and the whole package stayed green
			// (measured on the entry precondition).
			if code != 0 && code != 2 && code != 3 {
				t.Errorf("the runner exited %d over a map shape: a map it cannot use is a refusal (2) or "+
					"an incomplete gate (3), never a red tree (1)\n%s", code, out)
			}
			if code == 0 && !slices.Equal(declared, ran) {
				t.Errorf("the runner exited 0 having run %v, while the map declares %v — a green gate over a "+
					"member nobody ran is the defect this file exists for\n%s", ran, declared, out)
			}
			// And the sentence itself, because it is what the landing lane
			// reads: it may not appear unless the two sets agree.
			if strings.Contains(out, "green, and every required") && !slices.Equal(declared, ran) {
				t.Errorf("the runner announced a covered gate having run %v of %v:\n%s", ran, declared, out)
			}
		})
	}
}

// declaredMembers reads the members a map's author NAMED, generously and
// independently of the runner's yq: a sequence yields its scalars, a bare
// scalar yields itself (`members: first` means the member `first`), and
// anything else yields nothing a name can be made of. Deliberately not the
// runner's query — an oracle that shared the query would share its blind
// spots.
func declaredMembers(t *testing.T, gateMap string) []string {
	t.Helper()
	var doc struct {
		Jobs map[string]struct {
			Members yaml.Node `yaml:"members"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(gateMap), &doc); err != nil {
		t.Fatalf("the oracle cannot parse the map it is judging: %v", err)
	}
	var out []string
	for _, job := range doc.Jobs {
		switch job.Members.Kind {
		case yaml.ScalarNode:
			if job.Members.Value != "" && job.Members.Tag != "!!null" {
				out = append(out, job.Members.Value)
			}
		case yaml.SequenceNode:
			for _, n := range job.Members.Content {
				if n.Kind == yaml.ScalarNode && n.Value != "" && n.Tag != "!!null" {
					out = append(out, n.Value)
				}
			}
		}
	}
	return out
}

// TestTheMongoMemberRefusesAJobItCannotDerive covers the OTHER shipped
// member, and it exists because of what it caught: its invocations come from
// a `mapfile -t INVOCATIONS < <(python3 …)`, where the `|| { exit 2; }` reads
// MAPFILE's status, never python3's. The refusal the script computed and
// printed was thrown away, the loop ran zero invocations, and `exit "$failed"`
// answered 0 — the REQUIRED `mongo-conformance` context reporting green over
// nothing tested, with `ci:gate` concluding "every required or staged context
// is covered" on top of it.
//
// The derivation runs BEFORE the docker precondition precisely so this is
// testable on a machine with no docker: the member must be able to say "I
// cannot tell what to run" anywhere.
func TestTheMongoMemberRefusesAJobItCannotDerive(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		job        string
		realWF     bool
		deadDocker bool
		wantCode   int
		wantSaid   string
		alsoSaid   string
	}{
		{
			name:     "a job that runs no go test",
			job:      "    steps:\n      - name: start mongo\n        run: docker run -d mongo:8.0\n",
			wantCode: 2, wantSaid: "derived NO invocation",
		},
		{
			name:     "no mongo-conformance job at all",
			wantCode: 2, wantSaid: "derived NO invocation",
		},
		// The POSITIVE arm, and the one the two above cannot replace: a member
		// that refuses on EVERY workflow satisfies them both — measured, a
		// mutant that never derives anything left the package green. This one
		// runs the repository's REAL tests.yml against a docker whose daemon
		// is dead, so it holds three things at once: the derivation succeeds,
		// it finds BOTH `go test` lines (the second is the one
		// docs/development.md's regex cannot see, which is why the member
		// parses the block at all), and the refusal that follows is docker's
		// — which is what proves the derivation runs first.
		{
			name:   "the real workflow, with a dead docker daemon",
			realWF: true, deadDocker: true,
			wantCode: 2, wantSaid: "invocation(s) derived from",
			alsoSaid: "did not answer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			gittest.Run(t, root, "init", "-q")
			mustMkdir(t, filepath.Join(root, ".github", "workflows"))
			mustMkdir(t, filepath.Join(root, "scripts", "ci"))

			wf := "jobs:\n  other:\n    steps:\n      - run: echo hi\n"
			switch {
			case tc.realWF:
				wf = string(mustRead(t, workflowPaths[0]))
			case tc.job != "":
				wf = "jobs:\n  mongo-conformance:\n" + tc.job
			}
			mustWrite(t, filepath.Join(root, ".github", "workflows", "tests.yml"), wf, 0o644)

			body := mustRead(t, scriptUnderTest(t, "../../scripts/ci/mongo-conformance.sh"))
			script := filepath.Join(root, "scripts", "ci", "mongo-conformance.sh")
			mustWrite(t, script, string(body), 0o755)

			cmd := exec.Command("bash", script)
			cmd.Dir = root
			cmd.Env = append(scriptEnv(t), "ITERION_CI_GATE_EVIDENCE="+filepath.Join(root, "evidence"))
			if tc.deadDocker {
				// A docker that is PRESENT and whose daemon refuses — the
				// frequent shape, and the one `command -v docker` alone
				// cannot see.
				stub := filepath.Join(root, "stub")
				mustMkdir(t, stub)
				mustWrite(t, filepath.Join(stub, "docker"),
					"#!/bin/sh\necho 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock' >&2\nexit 1\n", 0o755)
				cmd.Env = append(cmd.Env, "PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			var buf strings.Builder
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			code := 0
			if err := cmd.Run(); err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("run the member: %v\n%s", err, buf.String())
				}
				code = exit.ExitCode()
			}
			if code != tc.wantCode {
				t.Errorf("the member exited %d, want %d — a member that cannot derive its invocations "+
					"and answers 0 is a required context reporting green over nothing\n%s", code, tc.wantCode, buf.String())
			}
			if !strings.Contains(buf.String(), tc.wantSaid) {
				t.Errorf("the member does not say %q:\n%s", tc.wantSaid, buf.String())
			}
			if tc.alsoSaid != "" && !strings.Contains(buf.String(), tc.alsoSaid) {
				t.Errorf("the member does not say %q — the refusal that follows a successful "+
					"derivation is what proves the order\n%s", tc.alsoSaid, buf.String())
			}
			if tc.realWF {
				// At least TWO, not an exact literal: the count is the job's
				// to change, and the floor is what the member exists for — a
				// member that found only the first `go test` would report a
				// verdict for a job it had not finished.
				m := derivedCount.FindStringSubmatch(buf.String())
				if m == nil {
					t.Fatalf("the member never said how many invocations it derived:\n%s", buf.String())
				}
				n, err := strconv.Atoi(m[1])
				if err != nil {
					t.Fatalf("unreadable derived count %q: %v", m[1], err)
				}
				if n < 2 {
					t.Errorf("the member derived %d invocation(s) from the real workflow: the job runs two "+
						"`go test` lines and the second is the one a line-oriented scan loses\n%s", n, buf.String())
				}
			}
		})
	}
}

// TestTheGofmtMemberJudgesWhatItSaysItJudges holds the third shipped member.
// It exists for the same reason as the two above: the only one of the three
// that had no witness was the one that answered 0 over nothing tested, and
// this member feeds the REQUIRED `test` aggregator. Three arms, because its
// two halves can each lie in a different direction — green over a file it
// never read, or red over a tree that is fine.
func TestTheGofmtMemberJudgesWhatItSaysItJudges(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// arrange runs in the scratch repo after `a.go` and `b.go` are
		// committed clean; it returns nothing and may use git.
		arrange  func(t *testing.T, root string)
		wantCode int
		wantSaid string
	}{
		{
			"a tracked file that needs gofmt",
			func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "a.go"), "package a\n\nfunc  A( ) {}\n", 0o644)
			},
			1, "a.go",
		},
		{
			// The false-red arm: an unstaged deletion made gofmt exit nonzero
			// with `lstat: no such file`, reported as "a Go file it cannot
			// parse" over a healthy committed tree.
			"a tracked file deleted from the worktree",
			func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, "b.go")); err != nil {
					t.Fatal(err)
				}
			},
			0, "absent from the worktree",
		},
		{
			// A dead symlink is NOT a deletion: `-e` follows the link and
			// would file it as one, green here while CI's `find` lists it and
			// gofmt fails on it. Judged, and gofmt's own error is the verdict.
			"a tracked Go file that is a dead symlink",
			func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, "b.go")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("missing-target.go", filepath.Join(root, "b.go")); err != nil {
					t.Fatal(err)
				}
			},
			1, "b.go",
		},
		{
			// The green-over-nothing arm: an empty list must refuse, because
			// `xargs -0` on empty input still invokes its command and
			// `gofmt -l` with no argument reads a closed stdin and exits 0.
			"no tracked Go file at all",
			func(t *testing.T, root string) {
				gittest.Run(t, root, "rm", "-q", "a.go", "b.go")
			},
			2, "not a clean tree",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			gittest.Run(t, root, "init", "-q")
			gittest.Run(t, root, "config", "user.email", "guard@example.test")
			gittest.Run(t, root, "config", "user.name", "guard")
			mustMkdir(t, filepath.Join(root, "scripts", "ci"))
			mustWrite(t, filepath.Join(root, "a.go"), "package a\n\nfunc A() {}\n", 0o644)
			mustWrite(t, filepath.Join(root, "b.go"), "package a\n\nfunc B() {}\n", 0o644)
			gittest.Run(t, root, "add", "a.go", "b.go")
			gittest.Run(t, root, "commit", "-qm", "seed")

			body := mustRead(t, scriptUnderTest(t, "../../scripts/ci/gofmt-check.sh"))
			script := filepath.Join(root, "scripts", "ci", "gofmt-check.sh")
			mustWrite(t, script, string(body), 0o755)

			tc.arrange(t, root)

			cmd := exec.Command("bash", script)
			cmd.Dir = root
			cmd.Env = scriptEnv(t)
			var buf strings.Builder
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			code := 0
			if err := cmd.Run(); err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("run the member: %v\n%s", err, buf.String())
				}
				code = exit.ExitCode()
			}
			if code != tc.wantCode {
				t.Errorf("the member exited %d, want %d\n%s", code, tc.wantCode, buf.String())
			}
			if !strings.Contains(buf.String(), tc.wantSaid) {
				t.Errorf("the member does not say %q:\n%s", tc.wantSaid, buf.String())
			}
			// Whatever the verdict, it must never claim a clean tree without
			// naming how many files it read.
			if code == 0 && !strings.Contains(buf.String(), "gofmt-clean") {
				t.Errorf("the member exited 0 without saying what it judged:\n%s", buf.String())
			}
		})
	}
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

// taskShape is what the runner needs to know about a member whose exit code
// it reads: how many ways it can fail. Three things decide that, and counting
// only `cmds:` gets two of them wrong — measured both directions. go-task
// accepts the SINGULAR `cmd:` and passes its code under `-x`, so a member
// written that way counted as zero commands and reddened for a race it does
// not have; and a single `cmds:` entry that DELEGATES (`- task: lint`) counts
// as one command while the task it calls carries parallel `deps:`, which is
// exactly the nondeterminism this shape exists to refuse (measured: 12 runs
// of `task -x` over two failing deps gave 2 7 7 2 7 7 7 2 2 2 2 7).
type taskShape struct {
	cmds, deps int
	delegates  bool
	// canSkip: go-task may answer 0 WITHOUT the command having run, or after
	// throwing its failure away — `ignore_error: true` turns every non-zero
	// into 0, and `status:`/`sources:` make it print "Task … is up to date"
	// and exit 0. Measured on the pinned 3.48.0 against a member declared in
	// `refusing_members`: both give `task -x` rc=0, the runner records 0 in
	// its matrix, and the gate prints "green, and every required or staged
	// context is covered" over a member whose failure it discarded or that it
	// never ran. No member carries one today (`brand:check`, `build` and
	// `studio:build` do, and none of them is a member — this slice is what
	// took `brand:check` out of a row).
	canSkip bool
	skipKey string
	// command is the task's single command, when it has exactly one and that
	// one is a scalar — the text properties 6 and 7 match against a job's
	// `run:`. Empty for a multi-command task, and for a delegation (a
	// mapping has no scalar text).
	command string
	// oneFailurePath: the exit code the runner reads can only have come from
	// one place.
	oneFailurePath bool
}

func (s taskShape) why() string {
	switch {
	case s.canSkip:
		return "`" + s.skipKey + "` lets go-task answer 0 without the command having failed"
	case s.delegates:
		return "its one command DELEGATES to another task, whose own deps decide the code"
	case s.deps > 0:
		return "its deps run in parallel, so the code is whichever of them failed first"
	default:
		return "it has more than one command, so the code is whichever failed first"
	}
}

// taskfileShapes reads each task's failure surface from the parsed mapping.
// Only the shape: what a command does is the task's business, how many ways
// it can fail is the gate's.
func taskfileShapes(t *testing.T) map[string]taskShape {
	t.Helper()
	var doc struct {
		Tasks map[string]struct {
			Cmds        []yaml.Node `yaml:"cmds"`
			Cmd         yaml.Node   `yaml:"cmd"`
			Deps        []yaml.Node `yaml:"deps"`
			IgnoreError bool        `yaml:"ignore_error"`
			Status      []yaml.Node `yaml:"status"`
			Sources     []yaml.Node `yaml:"sources"`
		} `yaml:"tasks"`
	}
	if err := yaml.Unmarshal(mustRead(t, taskfilePath), &doc); err != nil {
		t.Fatalf("parse %s: %v", taskfilePath, err)
	}
	shapes := make(map[string]taskShape, len(doc.Tasks))
	for name, task := range doc.Tasks {
		n := len(task.Cmds)
		if task.Cmd.Kind != 0 {
			n++
		}
		// A `cmds:` entry that is a MAPPING delegates only when it carries a
		// `task` key: `- cmd: <text>` and `- defer: <text>` are mappings too,
		// and calling those a delegation is a false red whose message names a
		// task that is not there.
		delegates := false
		for _, c := range task.Cmds {
			if c.Kind != yaml.MappingNode {
				continue
			}
			for i := 0; i+1 < len(c.Content); i += 2 {
				if c.Content[i].Value == "task" {
					delegates = true
				}
			}
		}
		canSkip, skipKey := false, ""
		switch {
		case task.IgnoreError:
			canSkip, skipKey = true, "ignore_error"
		case len(task.Status) > 0:
			canSkip, skipKey = true, "status"
		case len(task.Sources) > 0:
			canSkip, skipKey = true, "sources"
		}
		command := ""
		if n == 1 && !delegates {
			if len(task.Cmds) == 1 {
				command = strings.TrimSpace(task.Cmds[0].Value)
			} else {
				command = strings.TrimSpace(task.Cmd.Value)
			}
		}
		shapes[name] = taskShape{
			cmds:           n,
			deps:           len(task.Deps),
			delegates:      delegates,
			canSkip:        canSkip,
			skipKey:        skipKey,
			command:        command,
			oneFailurePath: n == 1 && len(task.Deps) == 0 && !delegates && !canSkip,
		}
	}
	return shapes
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

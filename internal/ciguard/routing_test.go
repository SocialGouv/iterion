package ciguard

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workflowPath is read once, from the file the workflow actually runs from.
const workflowPath = "../../.github/workflows/tests.yml"

// runsOnLine captures the value of every job-level `runs-on:` in the file.
var runsOnLine = regexp.MustCompile(`(?m)^\s{4}runs-on:\s*(.+)$`)

// TestSelfHostedRoutingExpressionsAgree pins every copy of one security
// decision against each other. The count is deliberately NOT asserted: a job
// added without the expression is the guard's blind spot by design — what it
// pins is that no copy DIFFERS, not how many exist.
//
// `runs-on` cannot read a workflow-level `env`, and routing the choice
// through a `needs:` job would put every job behind a single GitHub-hosted
// slot — the queue the self-hosted routing exists to escape. So the
// expression is repeated per job, and repetition on a security decision is
// how one copy quietly stops matching the others: a fork guard removed from
// five jobs is loud, removed from one it is invisible.
//
// The assertion is deliberately byte equality between the copies, not a
// parse of what each means. Any difference at all — a clause dropped, a bot
// login changed, an operator lever added to five of six — is a difference
// the reviewer must see.
func TestSelfHostedRoutingExpressionsAgree(t *testing.T) {
	src, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read the workflow the jobs run from: %v", err)
	}

	seen := map[string][]string{}
	for _, m := range runsOnLine.FindAllStringSubmatch(string(src), -1) {
		value := strings.TrimSpace(m[1])
		if !strings.Contains(value, "arc-runners") {
			continue // GitHub-hosted jobs carry a plain label, by design.
		}
		seen[value] = append(seen[value], value)
	}

	switch len(seen) {
	case 0:
		t.Fatal("no job routes to arc-runners — either the self-hosted routing was " +
			"removed (then delete this guard in the same change) or the expression " +
			"no longer names the scale set this reads for.")
	case 1:
		// One distinct expression: every copy agrees.
	default:
		t.Errorf("the %d jobs routed to arc-runners do NOT all carry the same expression, "+
			"so one security decision now has %d different answers. The fork guard and the "+
			"dependency-bot guard are only worth what their LEAST protected copy is worth.",
			total(seen), len(seen))
		for value, occurrences := range seen {
			t.Errorf("  %d copy/copies: %s", len(occurrences), value)
		}
	}
}

// TestSelfHostedRoutingKeepsItsThreeClauses asserts the expression still
// carries each guard the comment above it promises. Byte equality across
// copies (above) proves they AGREE; it cannot notice all six losing the
// same clause at once, which is exactly what a tidy-up does.
func TestSelfHostedRoutingKeepsItsThreeClauses(t *testing.T) {
	src, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read the workflow the jobs run from: %v", err)
	}

	var expression string
	for _, m := range runsOnLine.FindAllStringSubmatch(string(src), -1) {
		if value := strings.TrimSpace(m[1]); strings.Contains(value, "arc-runners") {
			expression = value
			break
		}
	}
	if expression == "" {
		t.Skip("no self-hosted routing to check; the guard above reports it")
	}

	for _, clause := range []struct{ needle, why string }{
		{"vars.CI_SELF_HOSTED", "the operator's lever — three REQUIRED checks route here, and " +
			"repairing by merging does not work when merging is what is broken"},
		{"head.repo.full_name != github.repository", "the fork guard — a public repository, and " +
			"these runners sit inside the cluster"},
		{"renovate[bot]", "the dependency-bot guard — its author has write access, its CONTENT " +
			"is arbitrary upstream code that `pnpm install` executes"},
	} {
		if !strings.Contains(expression, clause.needle) {
			t.Errorf("the self-hosted routing no longer carries %q.\nThat clause is %s.\n"+
				"If removing it is deliberate, delete this row in the same change and say why.",
				clause.needle, clause.why)
		}
	}
}

func total(m map[string][]string) int {
	n := 0
	for _, v := range m {
		n += len(v)
	}
	return n
}

// requiredChecks names the jobs THIS workflow defines that ruleset 18857412
// requires. It is a literal because the ruleset lives outside the repository
// and nothing here can read it.
//
// What that can and cannot catch, said plainly rather than implied:
//
//   - CAN: a required job deleted or renamed; a required job that skips the
//     merge queue; an advisory job that does not.
//   - CANNOT: whether the ruleset agrees with this list. `brand` sits here
//     before it sits in the ruleset, deliberately — the workflow half lands
//     first, the settings half follows.
//   - CANNOT: the CHECK-RUN NAME, which is what the ruleset actually keys on.
//     This map is keyed by job ID. A required job that gains a `name:` or a
//     matrix diverges from its id silently — `desktop-vet-cross` shows the
//     shape (its checks report as "desktop-vet-cross (windows)" and
//     "(darwin)", neither equal to the id). No required job has one today.
//   - CANNOT: a job whose STEPS are all skipped by a step-level `if:`. It
//     still reports SUCCESS.
//
// `revi/review` is required too but is posted by a bot, not by this file.
var requiredChecks = map[string]bool{
	"test":              true,
	"race":              true,
	"vendor-check":      true,
	"mongo-conformance": true,
	"golangci":          true,
	"brand":             true,
	// Here before it is in the ruleset, like `brand`: the job runs in the
	// merge queue from day one so the context exists the moment it is
	// promoted — a required check that only starts being reported when it
	// is promoted leaves the queue waiting for one that never arrives.
	"fmt-check": true,
}

// workflowJobs is the file PARSED, not scanned.
//
// The first version of this guard read `if:` with a line regex, and three
// legal spellings walked straight past it: a block scalar (`if: >-`), which
// let a REQUIRED job carry a merge-queue skip with the guard green — the exact
// catastrophe it exists to prevent; an INVERTED condition
// (`== 'merge_group'`), which reads as "skips" while meaning the opposite; and
// a job id containing `_` or an uppercase letter, which GitHub allows and the
// regex did not, so the job vanished AND its body was misattributed to the
// previous one, accusing an innocent job. A matcher of spellings never
// converges; the document has a parser, and it is already vendored.
type workflowJobs struct {
	Jobs map[string]struct {
		If string `yaml:"if"`
	} `yaml:"jobs"`
}

// skipsMergeGroup reports whether a job's `if:` carries the spelling the
// advisory jobs use to stay OUT of the merge queue. The polarity is the
// point: `!=` skips, `==` runs there and only there. A trailing comment is
// stripped first — it is prose, not condition.
//
// It matches a SPELLING, and that is all it can do: `github.event_name`
// `!='merge_group'` without spaces, an alternative, a `contains(…)` over a
// list all read here as "does not skip". Those misses are on the cheap side —
// an advisory job wrongly accused, or one really holding a queue slot. The
// expensive side is not left to it: a required check may carry no `if:` at
// all, refused outright below.
func skipsMergeGroup(cond string) bool {
	if i := strings.Index(cond, "#"); i >= 0 {
		cond = cond[:i]
	}
	cond = strings.Join(strings.Fields(cond), " ")
	return strings.Contains(cond, "github.event_name != 'merge_group'")
}

// TestEveryJobPicksASideOfTheMergeQueue holds the workflow header's
// load-bearing claim — "the jobs carrying that skip are exactly the complement
// of the ruleset's required checks" — to a test, because it was a comment and
// a comment does not hold.
//
// Both directions are a real defect, and each has happened:
//
//   - REQUIRED with any job-level `if:`: a job skipped by one reports
//     SUCCESS, so a required check that never ran satisfies the gate on every
//     queue entry. Silent, and it merges things nothing verified. The test
//     refuses the condition rather than judging it, because judging means
//     matching spellings and the spellings do not run out.
//   - ADVISORY without one: the job holds a slot in a queue that shares 20
//     concurrent runners across the organisation, to produce a verdict nobody
//     can act on. Measured: the `brand` job shipped that way and had to be
//     corrected in the round that followed.
func TestEveryJobPicksASideOfTheMergeQueue(t *testing.T) {
	src, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", workflowPath, err)
	}
	var wf workflowJobs
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPath, err)
	}
	if len(wf.Jobs) < 5 {
		t.Fatalf("parsed %d jobs from %s — the file no longer has the shape this guard assumes", len(wf.Jobs), workflowPath)
	}
	for name, job := range wf.Jobs {
		skips := skipsMergeGroup(job.If)
		switch {
		case requiredChecks[name] && strings.TrimSpace(job.If) != "":
			// Not "does it skip": whether a condition is true inside the
			// queue is a question this file READS rather than evaluates, and
			// reading loses. Measured: `github.event_name!='merge_group'` —
			// the advisory spelling with its two spaces removed, legal and
			// identical in meaning — walks past skipsMergeGroup, and so do an
			// alternative and a `contains(…)`. A widened matcher finds one
			// more spelling every round; the set is closed instead. Today all
			// six required jobs carry no `if:`, so this costs nothing.
			t.Errorf("job %q is a REQUIRED check and carries a job-level `if:` (%q) — a job its `if:` skips reports SUCCESS, so any condition false inside the merge queue satisfies the gate with a check that never ran. A required check carries no `if:`; if one truly must, teach this test to EVALUATE it before adding it here", name, job.If)
		case !requiredChecks[name] && !skips:
			t.Errorf("job %q is advisory and does NOT skip merge_group (if: %q) — it holds a queue slot for a verdict nobody can act on; add `if: github.event_name != 'merge_group'` verbatim, spaces included, or add it to the ruleset and to requiredChecks here", name, job.If)
		}
	}
	for name := range requiredChecks {
		if _, ok := wf.Jobs[name]; !ok {
			t.Errorf("requiredChecks names %q, which this workflow does not define — the ruleset would wait for a check that never reports", name)
		}
	}
}

// taskfilePath is the other half of the one command the `fmt-check` job
// runs. A task and a CI step that hold the same command in two places drift
// the day one of them gains a path or a flag, and nothing says so.
const taskfilePath = "../../Taskfile.yml"

type workflowSteps struct {
	Jobs map[string]struct {
		Steps []struct {
			Name string `yaml:"name"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

type taskfileTasks struct {
	Tasks map[string]struct {
		Cmds []yaml.Node `yaml:"cmds"`
	} `yaml:"tasks"`
}

// TestTheInlinedTasksMatchTheirTaskfileEntry holds a CI step that MIRRORS a
// task to the task itself. The repo inlines rather than installing go-task
// for one command, which is a choice, not a licence to let the two texts
// drift: `task fmt:check` is documented as "exactly what the job runs", and
// a doc sentence does not hold.
//
// The witness: change either side alone and this names the pair.
func TestTheInlinedTasksMatchTheirTaskfileEntry(t *testing.T) {
	mirrors := map[string]struct{ job, step, task string }{
		"fmt-check":  {"fmt-check", "Shipped bots and examples are canonical", "fmt:check"},
		"docs-build": {"docs-build", "Link checker self-test", "docs:links:test"},
	}
	wsrc, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", workflowPath, err)
	}
	var wf workflowSteps
	if err := yaml.Unmarshal(wsrc, &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPath, err)
	}
	tsrc, err := os.ReadFile(taskfilePath)
	if err != nil {
		t.Fatalf("read %s: %v", taskfilePath, err)
	}
	var tf taskfileTasks
	if err := yaml.Unmarshal(tsrc, &tf); err != nil {
		t.Fatalf("parse %s: %v", taskfilePath, err)
	}
	for label, m := range mirrors {
		job, ok := wf.Jobs[m.job]
		if !ok {
			t.Errorf("%s: the workflow has no job %q", label, m.job)
			continue
		}
		var run string
		for _, st := range job.Steps {
			if st.Name == m.step {
				run = strings.TrimSpace(st.Run)
			}
		}
		if run == "" {
			t.Errorf("%s: job %q has no step %q with a `run:`", label, m.job, m.step)
			continue
		}
		task, ok := tf.Tasks[m.task]
		if !ok || len(task.Cmds) != 1 {
			t.Errorf("%s: the Taskfile task %q is missing, or no longer one command", label, m.task)
			continue
		}
		if cmd := strings.TrimSpace(task.Cmds[0].Value); cmd != run {
			t.Errorf("%s: the CI step runs\n  %s\nand `task %s` runs\n  %s", label, run, m.task, cmd)
		}
	}
}

// skipsMergeGroup now answers the ADVISORY side alone, and it answers by
// matching a spelling. This table says which spellings — including one it
// does NOT recognise, recorded as a miss rather than dressed up as a
// property. On an advisory job a miss is a red test naming the exact text to
// write; on a required one it was the whole catastrophe, and that side is no
// longer decided here.
func TestSkipsMergeGroupRecognisesTheSpellingTheAdvisoryJobsUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		cond string
		want bool
	}{
		{"the negation the advisory jobs carry", "github.event_name != 'merge_group'", true},
		{"prose after the condition is not the condition", "github.event_name != 'merge_group' # advisory", true},
		{"the wrapper GitHub also accepts carries the same substring",
			"${{ github.event_name != 'merge_group' }}", true},
		{"no condition at all runs everywhere", "", false},
		{"the INVERTED condition runs there and only there", "github.event_name == 'merge_group'", false},
		{"an alternative re-admitting the queue does not skip it",
			"github.event_name == 'pull_request' || github.event_name == 'merge_group'", false},
		// Recorded as a MISS, not as a property: the same condition without
		// its spaces is legal and means the same thing, and this predicate
		// does not see it. Harmless on an advisory job — a red test naming
		// the exact spelling to write — and refused outright on a required
		// one by the test above, which is where it would have been costly.
		{"the same condition without spaces is not recognised",
			"github.event_name!='merge_group'", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := skipsMergeGroup(tc.cond); got != tc.want {
				t.Errorf("skipsMergeGroup(%q) = %v, want %v", tc.cond, got, tc.want)
			}
		})
	}
}

package ciguard

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workflowPaths are the files this guard reads. tests.yml carries the merge
// queue's required jobs; merge-queue-gate.yml mirrors the revi verdict onto
// the queue branch and carries the same self-hosted routing decision, so its
// copies are pinned against the others too.
var workflowPaths = []string{
	"../../.github/workflows/tests.yml",
	"../../.github/workflows/merge-queue-gate.yml",
}

// runsOnLine captures the value of every job-level `runs-on:` in a file.
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
	seen := map[string][]string{}
	for _, path := range workflowPaths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range runsOnLine.FindAllStringSubmatch(string(src), -1) {
			value := strings.TrimSpace(m[1])
			if !strings.Contains(value, "arc-runners") {
				continue // GitHub-hosted jobs carry a plain label, by design.
			}
			seen[value] = append(seen[value], value)
		}
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
	// Every DISTINCT expression across every guarded file must carry the
	// clauses — not just the first copy found: the header promises the guard
	// covers the class, and a second expression shape is exactly where a
	// clause would go missing.
	expressions := map[string]bool{}
	for _, path := range workflowPaths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range runsOnLine.FindAllStringSubmatch(string(src), -1) {
			if value := strings.TrimSpace(m[1]); strings.Contains(value, "arc-runners") {
				expressions[value] = true
			}
		}
	}
	if len(expressions) == 0 {
		t.Skip("no self-hosted routing to check; the guard above reports it")
	}

	for expression := range expressions {
		for _, clause := range []struct{ needle, why string }{
			{"vars.CI_SELF_HOSTED", "the operator's lever — REQUIRED checks route here, and " +
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
// `test` and `race` are AGGREGATORS: the ruleset requires those NAMES, and
// renaming either job would hang every queue entry on a check that never
// reports — so the names survive while the real work moved to the gated
// legs below (gatedJobs), each aggregator `needs:`-ing its legs and
// re-verifying their conclusions under `if: ${{ !cancelled() }}`.
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
	// Here before it is in the ruleset: the job runs in the
	// merge queue from day one so the context exists the moment it is
	// promoted — a required check that only starts being reported when it
	// is promoted leaves the queue waiting for one that never arrives.
	"fmt-check": true,
}

// gatedJobs are the workflow's gated legs: they run in the merge queue
// behind a required aggregator and are deliberately NOT required themselves —
// the aggregator speaks for them. The contract this bucket enforces:
//
//   - NO job-level `if:` of any kind. A leg an `if:` skips reports SUCCESS on
//     the pull request (the guarantee dies, silently) while in the queue its
//     aggregator re-verifies a `skipped` into a red verdict — a queue entry
//     that can never pass, forever.
//   - SOME required aggregator must `needs:` it (TestEveryGatedJobIsNeededByA
//     RequiredAggregator) — an orphan leg is red on pull requests only, and
//     the queue would stay green on work nothing aggregates.
var gatedJobs = map[string]bool{
	"test-unit":  true,
	"test-e2e":   true,
	"studio":     true,
	"race-shard": true,
}

// aggregatorIf is the ONE evaluated spelling a required aggregator may carry,
// and the only `if:` any required job is allowed at all. It reads exclusively
// run-cancellation state — a value that cannot differ inside a merge_group
// run — unlike every event-dependent condition, whose queue-truth this file
// would have to READ rather than compare (and reading conditions loses; see
// TestEveryJobPicksASideOfTheMergeQueue).
const aggregatorIf = "${{ !cancelled() }}"

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
		If              string            `yaml:"if"`
		Needs           []string          `yaml:"needs"`
		Env             map[string]string `yaml:"env"`
		ContinueOnError bool              `yaml:"continue-on-error"`
		Strategy        struct {
			Matrix map[string][]yaml.Node `yaml:"matrix"` // Node, not typed: an axis may carry strings; only its length matters to the shard-count guard
		} `yaml:"strategy"`
		Steps []struct {
			Name string `yaml:"name"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
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
	src, err := os.ReadFile(workflowPaths[0])
	if err != nil {
		t.Fatalf("read %s: %v", workflowPaths[0], err)
	}
	var wf workflowJobs
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	if len(wf.Jobs) < 5 {
		t.Fatalf("parsed %d jobs from %s — the file no longer has the shape this guard assumes", len(wf.Jobs), workflowPaths[0])
	}
	for name, job := range wf.Jobs {
		skips := skipsMergeGroup(job.If)
		switch {
		case (requiredChecks[name] || gatedJobs[name]) && job.ContinueOnError:
			// A job-level continue-on-error reports SUCCESS with failing
			// steps — the needs-graph AND the aggregator's re-verification
			// both read that success. It neutralises every guard in this
			// file at once, so it is refused rather than judged.
			t.Errorf("job %q carries continue-on-error: true — a failing job would report success to its aggregator and to the ruleset. Nothing that feeds a required verdict may carry it", name)
		case requiredChecks[name] && strings.TrimSpace(job.If) != "" && strings.TrimSpace(job.If) != aggregatorIf:
			// A required job carries either NO `if:` or exactly the
			// aggregator spelling. Anything else is refused outright, not
			// judged: whether a condition is true inside the queue is a
			// question this file READS rather than evaluates, and reading
			// loses. Measured: `github.event_name!='merge_group'` — the
			// advisory spelling with its two spaces removed, legal and
			// identical in meaning — walks past skipsMergeGroup, and so do
			// an alternative and a `contains(…)`. A widened matcher finds
			// one more spelling every round; the set is closed instead.
			t.Errorf("job %q is a REQUIRED check and carries a job-level `if:` (%q) — a job its `if:` skips reports SUCCESS, so any condition false inside the merge queue satisfies the gate with a check that never ran. A required check carries no `if:`, except an aggregator's exactly %q", name, job.If, aggregatorIf)
		case gatedJobs[name] && strings.TrimSpace(job.If) != "":
			// A leg with an `if:` is worse on both sides: skipped on the
			// pull request (SUCCESS — the guarantee dies silently) while in
			// the queue its aggregator re-verifies a `skipped` result into a
			// red verdict the entry can never clear.
			t.Errorf("job %q is a gated leg and carries a job-level `if:` (%q) — legs run everywhere their aggregator runs, or not at all. Delete the condition, or rename the job out of gatedJobs if it no longer feeds an aggregator", name, job.If)
		case !requiredChecks[name] && !gatedJobs[name] && !skips:
			t.Errorf("job %q is advisory and does NOT skip merge_group (if: %q) — it holds a queue slot for a verdict nobody can act on; add `if: github.event_name != 'merge_group'` verbatim, spaces included, or add it to the ruleset and to requiredChecks here", name, job.If)
		}
	}
	for name := range requiredChecks {
		if _, ok := wf.Jobs[name]; !ok {
			t.Errorf("requiredChecks names %q, which this workflow does not define — the ruleset would wait for a check that never reports", name)
		}
	}
	for name := range gatedJobs {
		if _, ok := wf.Jobs[name]; !ok {
			t.Errorf("gatedJobs names %q, which this workflow does not define — the leg bucket drifted from the file", name)
		}
	}
}

// TestEveryGatedJobIsNeededByARequiredAggregator closes the orphan-leg hole:
// a gated leg some required aggregator does not `needs:` is red on pull
// requests only — the merge queue, the only merge that matters, would stay
// green on work nothing aggregates.
func TestEveryGatedJobIsNeededByARequiredAggregator(t *testing.T) {
	var wf workflowJobs
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	for name := range gatedJobs {
		needed := false
		for jobName, job := range wf.Jobs {
			if requiredChecks[jobName] {
				for _, dep := range job.Needs {
					if dep == name {
						needed = true
					}
				}
			}
		}
		if !needed {
			t.Errorf("gated leg %q is not named in any required job's needs: — it runs on pull requests while the merge queue never sees its verdict. Add it to its aggregator's needs:, or move it out of gatedJobs", name)
		}
	}
}

// TestAggregatorsReVerifyEveryLeg pins the aggregator pattern's three
// load-bearing properties, because the trap they answer reports SUCCESS, not
// red:
//
//  1. every job that `needs:` something carries exactly the aggregator `if:` —
//     a plain needs-gated job is SKIPPED when a leg fails, and a skipped check
//     reports SUCCESS;
//  2. everything it needs is a gated leg — a `needs:` on an advisory job
//     would read `skipped` in the queue, where advisories do not run;
//  3. every leg's `needs.<leg>.result` is read into the job's env, and the
//     steps compare against `success` — the re-verification that turns any
//     leg outcome other than success into a red required check.
func TestAggregatorsReVerifyEveryLeg(t *testing.T) {
	var wf workflowJobs
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	for name, job := range wf.Jobs {
		if len(job.Needs) == 0 {
			continue
		}
		if strings.TrimSpace(job.If) != aggregatorIf {
			t.Errorf("job %q needs %v but carries `if:` %q — with plain needs, a failed leg SKIPS this job and a skipped check reports SUCCESS. Carry exactly %s", name, job.Needs, job.If, aggregatorIf)
		}
		var stepText strings.Builder
		for _, st := range job.Steps {
			stepText.WriteString(st.Run)
			stepText.WriteString("\n")
		}
		for _, leg := range job.Needs {
			if !gatedJobs[leg] {
				t.Errorf("job %q needs %q, which is not a gated leg — an advisory member reports `skipped` inside the merge queue and the aggregator would re-verify it into red", name, leg)
			}
			needle := "needs." + leg + ".result"
			envKey := ""
			for key, v := range job.Env {
				if strings.Contains(v, needle) {
					envKey = key
				}
			}
			if envKey == "" {
				t.Errorf("job %q needs %q but never reads %s into its env — a leg whose result is not re-verified could report anything", name, leg, needle)
				continue
			}
			// Per-leg, not once for the whole job: the comparison that makes
			// a red leg a red REQUIRED check must involve THIS leg's own
			// value. A shared `"success"` needle satisfied by any other
			// leg's line let a dropped member re-verify nothing while the
			// suite stayed green (measured).
			if !strings.Contains(stepText.String(), envKey) {
				t.Errorf("job %q reads %q into env but its steps never reference %s — leg %q is collected and then ignored, so its failure would not redden the required check", name, needle, envKey, leg)
			}
		}
	}
}

// TestShardMatrixMatchesTheSplitterCount pins the race matrix to the
// partition width. The two numbers live in different corners of the job
// (strategy vs step) and nothing else relates them: a matrix widened past
// `-n` fails loud (the splitter refuses -i >= n), but a `-n` raised past the
// matrix silently drops the highest shard from race coverage — a quarter of
// the suite, never under the detector again, on every commit after. Measured:
// both suites stayed green under exactly that drift.
func TestShardMatrixMatchesTheSplitterCount(t *testing.T) {
	var wf workflowJobs
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	var splitterN = regexp.MustCompile(`-n (\d+)`)
	checked := 0
	for name, job := range wf.Jobs {
		if !gatedJobs[name] || len(job.Strategy.Matrix) == 0 {
			continue
		}
		for _, steps := range job.Steps {
			for _, m := range splitterN.FindAllStringSubmatch(steps.Run, -1) {
				n := 0
				fmt.Sscanf(m[1], "%d", &n)
				for axis, values := range job.Strategy.Matrix {
					if len(values) != n {
						t.Errorf("job %q: matrix %s has %d values but its splitter runs -n %d — the widest shard index would silently never run", name, axis, len(values), n)
					}
					checked++
				}
			}
		}
	}
	if checked == 0 {
		t.Skip("no sharded leg to check")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return src
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
	wsrc, err := os.ReadFile(workflowPaths[0])
	if err != nil {
		t.Fatalf("read %s: %v", workflowPaths[0], err)
	}
	var wf workflowSteps
	if err := yaml.Unmarshal(wsrc, &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
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

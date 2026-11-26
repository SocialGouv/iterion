package ciguard

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
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

// The three routing classes a self-hosted job belongs to. Every class
// expression shares every clause byte for byte and differs from the others in
// the variable of its last alternative only, each ending on
// `vars.CI_<CLASS>_RUNNER || 'arc-runners'`: the repository variable names the
// class's scale set, and while it is unset the job lands on `arc-runners`, so
// an unset variable changes no routing.
//
//   - small: aggregators and seconds-long scripts;
//   - large: the heavy jobs the merge queue requires (test-unit, golangci,
//     studio);
//   - heavy: the non-required heavy layer, which runs on push to main, the
//     nightly, a dispatch and `ci:full` (ADR-173).
const (
	smallRunner = "small"
	largeRunner = "large"
	heavyRunner = "heavy"

	// routingTailHead opens the last alternatives of every routing
	// expression, the self-hosted one and test-native-exec's alike: what
	// precedes it is the shared base, the clauses.
	routingTailHead = "&& 'ubuntu-latest' || "
)

// runnerClassOrder lists the classes in a stable order for reports.
var runnerClassOrder = []string{smallRunner, largeRunner, heavyRunner}

// routingTails are the three canonical tails, one per class.
var routingTails = map[string]string{
	smallRunner: routingTailHead + "vars.CI_SMALL_RUNNER || 'arc-runners' }}",
	largeRunner: routingTailHead + "vars.CI_LARGE_RUNNER || 'arc-runners' }}",
	heavyRunner: routingTailHead + "vars.CI_HEAVY_RUNNER || 'arc-runners' }}",
}

// routingVariables are the repository variables that send a job to a
// self-hosted scale set; any of them in a `runs-on` makes the job self-hosted
// for this guard, whatever else the expression says.
var routingVariables = []string{"CI_SMALL_RUNNER", "CI_LARGE_RUNNER", "CI_HEAVY_RUNNER"}

// runnerClasses pins, per workflow file and job id, which class each
// self-hosted job routes to. It is the classification of #3694, measured on
// peak memory and CPU: a job on the wrong expression either starves on 500m
// or holds a large pod for a seconds-long script. A self-hosted job absent
// from this table is red: a new job is classified, never silently accepted.
// test-native-exec carries its own runner variable and is pinned by
// native_exec_runner_test.go; GitHub-hosted jobs are not listed.
var runnerClasses = map[string]map[string]string{
	"../../.github/workflows/tests.yml": {
		"test-unit": largeRunner,
		"studio":    largeRunner,
		"golangci":  largeRunner,

		"race-shard":        heavyRunner,
		"test-e2e":          heavyRunner,
		"mongo-conformance": heavyRunner,
		"cloud-e2e":         heavyRunner,

		"test":              smallRunner,
		"race":              smallRunner,
		"nats-conformance":  largeRunner,
		"vendor-check":      smallRunner,
		"helm-lint":         smallRunner,
		"brand":             smallRunner,
		"desktop-vet-cross": smallRunner,
		"govulncheck":       smallRunner,
		"fmt-check":         smallRunner,
		"docs-build":        smallRunner,
	},
	"../../.github/workflows/merge-queue-gate.yml": {
		"mirror-revi-verdict": smallRunner,
	},
}

// routedJob is one job-level `runs-on:` that reaches a self-hosted runner.
type routedJob struct {
	path, id, runsOn string
}

// namesSelfHostedRunner reports whether a `runs-on` value can reach a
// self-hosted scale set: it names `arc-runners` or a routing variable.
func namesSelfHostedRunner(runsOn string) bool {
	// GitHub resolves `vars` names and runner labels case-insensitively, so
	// `vars.ci_heavy_runner` routes like `vars.CI_HEAVY_RUNNER`.
	if strings.Contains(strings.ToLower(runsOn), "arc-runners") {
		return true
	}
	for _, v := range routingVariables {
		if strings.Contains(strings.ToUpper(runsOn), v) {
			return true
		}
	}
	return false
}

// selfHostedJobs reads every job of workflowPaths whose `runs-on:` names the
// scale set or a routing variable.
func selfHostedJobs(t *testing.T) []routedJob {
	t.Helper()
	var jobs []routedJob
	for _, path := range workflowPaths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var wf struct {
			Jobs map[string]struct {
				RunsOn string `yaml:"runs-on"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(src, &wf); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for id, job := range wf.Jobs {
			value := strings.TrimSpace(job.RunsOn)
			if namesSelfHostedRunner(value) {
				jobs = append(jobs, routedJob{path: path, id: id, runsOn: value})
			}
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].path != jobs[j].path {
			return jobs[i].path < jobs[j].path
		}
		return jobs[i].id < jobs[j].id
	})
	return jobs
}

// routingClass names the class an expression's tail selects, or "" for a
// spelling that is none of the canonical tails.
func routingClass(runsOn string) string {
	for _, class := range runnerClassOrder {
		if strings.HasSuffix(runsOn, routingTails[class]) {
			return class
		}
	}
	return ""
}

// routingBase is an expression with its canonical class tail removed: the
// clauses every class shares. ok is false for a non-canonical spelling.
func routingBase(runsOn string) (base string, ok bool) {
	class := routingClass(runsOn)
	if class == "" {
		return "", false
	}
	return strings.TrimSuffix(runsOn, routingTails[class]), true
}

// TestSelfHostedRoutingExpressionsAgree pins every copy of one security
// decision against each other, in exactly three canonical spellings — small,
// large and heavy — and pins which job carries which.
//
// `runs-on` cannot read a workflow-level `env`, and routing the choice
// through a `needs:` job would put every job behind a single GitHub-hosted
// slot — the queue the self-hosted routing exists to escape. So the
// expression is repeated per job, and repetition on a security decision is
// how one copy quietly stops matching the others: a fork guard removed from
// five jobs is loud, removed from one it is invisible.
//
// The assertion is deliberately byte equality, not a parse of what each
// copy means: within a class every copy is identical, and the three class
// expressions are one base with only the tail swapped. Any other difference
// — a clause dropped, a bot login changed, an operator lever added to five of
// six — is a difference the reviewer must see.
func TestSelfHostedRoutingExpressionsAgree(t *testing.T) {
	jobs := selfHostedJobs(t)
	if len(jobs) == 0 {
		t.Fatal("no job routes to arc-runners — either the self-hosted routing was " +
			"removed (then delete this guard in the same change) or the expression " +
			"no longer names the scale set this reads for.")
	}

	seen := map[string]map[string]bool{}
	byClass := map[string]map[string][]string{}
	for _, class := range runnerClassOrder {
		byClass[class] = map[string][]string{}
	}
	for _, job := range jobs {
		if seen[job.path] == nil {
			seen[job.path] = map[string]bool{}
		}
		seen[job.path][job.id] = true
		want, classified := runnerClasses[job.path][job.id]
		if !classified {
			t.Errorf("%s: job %q routes to a self-hosted runner but runnerClasses does not "+
				"classify it: add it as one of %q, from its measured peak", job.path, job.id, runnerClassOrder)
			continue
		}
		got := routingClass(job.runsOn)
		switch {
		case got == "":
			t.Errorf("%s: job %q carries a routing spelling outside the three canonical tails %q:\n  %s",
				job.path, job.id, routingTails, job.runsOn)
			continue
		case got != want:
			t.Errorf("%s: job %q is classified %s but carries the %s expression", job.path, job.id, want, got)
		}
		byClass[got][job.runsOn] = append(byClass[got][job.runsOn], job.id)
	}
	for path, classes := range runnerClasses {
		for id, class := range classes {
			if _, known := routingTails[class]; !known {
				t.Errorf("%s: job %q is classified %q, which is none of %q", path, id, class, runnerClassOrder)
			}
			if !seen[path][id] {
				t.Errorf("%s: job %q is classified %s but no longer routes to a self-hosted runner "+
					"(renamed, deleted or moved to a hosted label): update runnerClasses in the same change", path, id, class)
			}
		}
	}

	bases := map[string][]string{}
	for _, class := range runnerClassOrder {
		switch len(byClass[class]) {
		case 0:
			// A class with no job is reported per job above whenever runnerClasses expects one.
		case 1:
			for value := range byClass[class] {
				base, _ := routingBase(value)
				bases[base] = append(bases[base], class)
			}
		default:
			t.Errorf("the %s jobs do NOT all carry the same expression, so one security decision "+
				"now has %d different answers. The fork guard and the dependency-bot guard are only "+
				"worth what their LEAST protected copy is worth.", class, len(byClass[class]))
			for value, ids := range byClass[class] {
				t.Errorf("  %v: %s", ids, value)
			}
		}
	}
	if len(bases) > 1 {
		t.Errorf("the class expressions differ in more than their last alternative: %d different bases", len(bases))
		for base, classes := range bases {
			t.Errorf("  %v: %s", classes, base)
		}
	}
}

// TestSelfHostedRoutingKeepsItsClauses asserts the expression still
// carries each guard the comment above it promises. Byte equality across
// copies (above) proves they AGREE; it cannot notice all six losing the
// same clause at once, which is exactly what a tidy-up does.
func TestSelfHostedRoutingKeepsItsClauses(t *testing.T) {
	// Every DISTINCT expression across every guarded file must carry the
	// clauses — not just the first copy found: the header promises the guard
	// covers the class, and a second expression shape is exactly where a
	// clause would go missing.
	expressions := map[string]bool{}
	for _, job := range selfHostedJobs(t) {
		expressions[job.runsOn] = true
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
			{"dependabot[bot]", "the retired-bot carve-out — Dependabot is off here, and the " +
				"literal covers that lane before it reopens"},
		} {
			if !strings.Contains(expression, clause.needle) {
				t.Errorf("the self-hosted routing no longer carries %q.\nThat clause is %s.\n"+
					"If removing it is deliberate, delete this row in the same change and say why.",
					clause.needle, clause.why)
			}
		}
	}
}

// requiredChecks names the jobs THIS workflow defines that ruleset 18857412
// requires. It is a literal because the ruleset lives outside the repository
// and nothing here can read it.
//
// `test` is an AGGREGATOR: the ruleset requires that NAME, and renaming the
// job would hang every queue entry on a check that never reports — so the
// name survives while the real work moved to the gated legs below
// (gatedJobs), the aggregator `needs:`-ing its legs and re-verifying their
// conclusions under aggregatorIf — `!cancelled()` combined with the tap skip
// its legs carry. `race` and `mongo-conformance` are heldChecks, out of the
// ruleset for the experimental delivery mode.
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
	"test":         true,
	"vendor-check": true,
	"golangci":     true,
	"brand":        true,
	// Here before it is in the ruleset: the job runs in the
	// merge queue from day one so the context exists the moment it is
	// promoted — a required check that only starts being reported when it
	// is promoted leaves the queue waiting for one that never arrives.
	"fmt-check": true,
}

// heldChecks are the contexts ruleset 18857412 required until the
// experimental delivery mode (docs/adr/173-experimental-delivery-mode.md,
// #3691) took them out of the merge queue, and requires again when the mode
// ends. They keep reporting so the ruleset can name them back: `race` is an
// aggregator of `test`'s shape over race-shard, held to the same contract
// (aggregatorIf, every leg re-verified, a heavy leg's skip forgiven on a
// light run only); `mongo-conformance` is a heavy advisory job. Ending the
// mode moves them back into requiredChecks AND takes `merge_group` out of
// the heavy literal in the same change: a held check named by the ruleset
// while the queue skips its work is green on a check that never ran.
var heldChecks = map[string]bool{
	"race":              true,
	"mongo-conformance": true,
}

// gatedJobs are the workflow's gated legs: they run in the merge queue
// behind a required aggregator and are deliberately NOT required themselves —
// the aggregator speaks for them. The contract this bucket enforces:
//
//   - Exactly tapSkip as its job-level `if:`, and nothing else — the one
//     verified condition its aggregator carries too, so the leg skips where
//     the aggregator skips and runs everywhere it runs. A leg any other
//     `if:` skips reports SUCCESS on the pull request (the guarantee dies,
//     silently) while in the queue its aggregator re-verifies a `skipped`
//     into a red verdict — a queue entry that can never pass, forever. A leg
//     without the skip spends the heavy minutes the skip exists to save.
//   - SOME required aggregator must `needs:` it (TestEveryGatedJobIsNeededByA
//     RequiredAggregator) — an orphan leg is red on pull requests only, and
//     the queue would stay green on work nothing aggregates.
var gatedJobs = map[string]bool{
	"test-unit":        true,
	"test-e2e":         true,
	"studio":           true,
	"test-native-exec": true,
	"race-shard":       true,
}

// fullSuite is true wherever the heavy layer runs: on every event that is
// neither a pull request, a merge-queue build nor a push — the nightly
// schedule, a dispatch, and any trigger added later, so a new trigger runs
// the full suite by default — on a push heavyGateJob did not decide light
// (the debounced push, #3737: only the exact output `false` is light, so a
// gate that failed or never answered runs the heavy layer), and on a pull
// request that carries the `ci:full` opt-in label. A merge-queue build, an
// unlabelled pull request and a push inside the heavy window are LIGHT runs:
// the heavy layer skips them (#3683 for the pull request, #3691 for the
// queue, the experimental delivery mode). One literal, three
// readers, each pinned byte for byte below: the heavy legs' `if:`
// (heavyLegIf), the heavy advisory jobs' `if:` (heavyAdvisoryIf) and the
// aggregators' lightRunEnv. The legs run where it is true and the
// aggregators forgive a `skipped` leg where its negation is true, so the two
// sides cannot disagree about which runs are light; event_layers_test.go
// evaluates all three, per event.
const fullSuite = `((github.event_name != 'pull_request' && github.event_name != 'merge_group' && (github.event_name != 'push' || needs.heavy-gate.outputs.heavy != 'false')) || contains(github.event.pull_request.labels.*.name, 'ci:full'))`

// heavyLegs are the gated legs a light run skips: the -race shards and the
// -race e2e shards, seven ARC pods of up to 50 minutes. Each still feeds its
// aggregator on push, on the nightly and on a `ci:full` pull request.
var heavyLegs = map[string]bool{
	"test-e2e":   true,
	"race-shard": true,
}

// heavyLegIf is the one `if:` a heavy leg carries: the tap skip every gated
// leg carries, combined with fullSuite, behind `!cancelled()` — a heavy leg
// `needs:` heavyGateJob to read its output, and without a status function a
// job whose need skipped (every event but push) or failed is skipped too.
const heavyLegIf = "${{ !cancelled() && " + fullSuite + " && " + tapSkip + " }}"

// heavyAdvisoryJobs are the heavy jobs no aggregator speaks for: they skip
// every light run, the merge queue included, and run with the heavy legs.
var heavyAdvisoryJobs = map[string]bool{
	"cloud-e2e":         true,
	"mongo-conformance": true,
}

// heavyAdvisoryIf is the one `if:` a heavy advisory job carries: fullSuite
// alone, which already keeps it out of the merge queue, behind the same
// `!cancelled()` as heavyLegIf and for the same reason.
const heavyAdvisoryIf = "${{ !cancelled() && " + fullSuite + " }}"

// heavyGateJob is the decider of the debounced push (#3737): on a push to
// main it answers `heavy=true` or `heavy=false` from the run history, and
// fullSuite reads that output under `github.event_name == 'push'` only. It
// runs on push and nowhere else (heavyGateIf), so the merge queue never
// spends a slot on it. Every job whose `if:` or env reads fullSuite `needs:`
// it; it is no leg — no aggregator re-verifies its result, because where it
// fails its output is empty and the run is heavy.
const (
	heavyGateJob = "heavy-gate"
	heavyGateIf  = "github.event_name == 'push'"
)

// lightRunEnv is the env key, and lightRunValue its exact value, through
// which an aggregator learns whether the run is light: the negation of
// fullSuite, evaluated by GitHub, so it is `true` in merge_group and on an
// unlabelled pull request and `false` on push, schedule, dispatch and a
// `ci:full` pull request, and the aggregator's step accepts a `skipped` heavy
// leg only when it reads `true`.
const (
	lightRunEnv   = "LIGHT_RUN"
	lightRunValue = "${{ !" + fullSuite + " }}"
)

// aggregatorIf is the ONE spelling a required aggregator may carry: the
// run-state clause `!cancelled()` — it reads exclusively run-cancellation
// state, a value that cannot differ inside a merge_group run, unlike every
// event-dependent condition, whose queue-truth this file would have to READ
// rather than compare (and reading conditions loses; see
// TestEveryJobPicksASideOfTheMergeQueue) — combined with tapSkip, the one
// verified condition its legs carry, so the aggregator skips exactly the
// entries its legs skip and re-verifies every other one. Byte for byte: the
// tap half is the same literal the heavy jobs and the verifier share.
const aggregatorIf = "${{ !cancelled() && " + tapSkip + " }}"

// aggregatedLegs returns the legs a job's `needs:` names, heavyGateJob left
// out: the decider is needed for its output, never re-verified as a leg. A
// job whose legs are empty is no aggregator, whatever else it needs.
func aggregatedLegs[T any](needs []T) []string {
	var legs []string
	for _, n := range needs {
		if leg := fmt.Sprint(n); leg != heavyGateJob {
			legs = append(legs, leg)
		}
	}
	return legs
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
		If              string            `yaml:"if"`
		Needs           []string          `yaml:"needs"`
		Env             map[string]string `yaml:"env"`
		ContinueOnError bool              `yaml:"continue-on-error"`
		Strategy        struct {
			Matrix map[string][]yaml.Node `yaml:"matrix"` // Node, not typed: an axis may carry strings; only its length matters to the shard-count guard
		} `yaml:"strategy"`
		Steps []struct {
			Name            string `yaml:"name"`
			Run             string `yaml:"run"`
			ContinueOnError any    `yaml:"continue-on-error"`
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
//     matching spellings and the spellings do not run out. Two conditions
//     are admitted, each compared byte for byte rather than read, each tied
//     to the role of the job that carries it: tapSkip on a required job that
//     does its own work (and on every gated leg), whose verifier
//     TestTheTapSkipIsVerifiedWhereNothingSkipsIt holds in place; and
//     aggregatorIf on a required aggregator — the same tap skip combined
//     with `!cancelled()`, the trio TestAggregatorsReVerifyEveryLeg holds.
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
		case (requiredChecks[name] || heldChecks[name] || gatedJobs[name]) && job.ContinueOnError:
			// A job-level continue-on-error reports SUCCESS with failing
			// steps — the needs-graph AND the aggregator's re-verification
			// both read that success. It neutralises every guard in this
			// file at once, so it is refused rather than judged.
			t.Errorf("job %q carries continue-on-error: true — a failing job would report success to its aggregator and to the ruleset. Nothing that feeds a required verdict may carry it", name)
		case requiredChecks[name] && name != tapVerifierJob && len(job.Needs) == 0 && sameExpression(job.If, tapSkip):
			// The one admitted condition on a required job that does its own
			// work: a Homebrew tap update skips the heavy jobs, and the
			// verifier that makes it safe lives in a job that can never
			// carry it.
		case (requiredChecks[name] || heldChecks[name]) && len(aggregatedLegs(job.Needs)) > 0 && sameExpression(job.If, aggregatorIf):
			// The one admitted condition on a required or held AGGREGATOR: the
			// run-state spelling that keeps a failed leg from skipping it,
			// combined with the same tap skip its legs carry — it skips
			// exactly the entries they skip and re-verifies every other one.
			// TestAggregatorsReVerifyEveryLeg holds the rest of the trio.
		case requiredChecks[name] && strings.TrimSpace(job.If) != "":
			// Anything else is refused outright, not judged: whether a
			// condition is true inside the queue is a question this file
			// READS rather than evaluates, and reading loses. Measured:
			// `github.event_name!='merge_group'` — the advisory spelling with
			// its two spaces removed, legal and identical in meaning — walks
			// past skipsMergeGroup, and so do an alternative and a
			// `contains(…)`. A widened matcher finds one more spelling every
			// round; the set is closed instead, and each member is compared
			// byte for byte against the role of the job that carries it.
			t.Errorf("job %q is a REQUIRED check and carries a job-level `if:` (%q) — a job its `if:` skips reports SUCCESS, so any condition false inside the merge queue satisfies the gate with a check that never ran. A required check carries no `if:` other than tapSkip (%q), byte for byte, or on an aggregator exactly %q; %q carries none at all. If another one truly must, teach this test to EVALUATE it before adding it here", name, job.If, tapSkip, aggregatorIf, tapVerifierJob)
		case heavyLegs[name] && !gatedJobs[name]:
			t.Errorf("job %q is a heavy leg but not a gated leg: heavyLegs is a subset of gatedJobs", name)
		case heavyLegs[name] && sameExpression(job.If, heavyLegIf):
			// A heavy leg skips a light run on top of the tap skip; its
			// aggregator forgives that `skipped` through lightRunEnv only,
			// which reads `false` on push, schedule, dispatch and `ci:full`
			// (event_layers_test.go evaluates it per event).
		case heavyLegs[name]:
			t.Errorf("job %q is a heavy leg and carries `if:` %q, want exactly %q — the light-run skip and the tap skip, nothing else. A heavy leg without the light skip spends its ARC pods on every pull request push and queue build (#3683, #3691); with any other condition, its aggregator's forgiveness and its skip read different runs", name, job.If, heavyLegIf)
		case gatedJobs[name] && sameExpression(job.If, tapSkip):
			// A leg carries the tap skip its aggregator carries: it runs
			// everywhere its aggregator runs and skips exactly where the
			// aggregator skips with it.
		case gatedJobs[name]:
			// Any other `if:` on a leg is worse on both sides: skipped on the
			// pull request (SUCCESS — the guarantee dies silently) while in
			// the queue its aggregator re-verifies a `skipped` result into a
			// red verdict the entry can never clear. And a leg WITHOUT the
			// skip spends, on every tap entry, the heavy minutes the skip
			// exists to save — verified by nobody, since its aggregator
			// skipped.
			t.Errorf("job %q is a gated leg and carries `if:` %q, want exactly tapSkip (%q) — legs run everywhere their aggregator runs and skip exactly where it skips. Carry the tap skip and nothing else, or rename the job out of gatedJobs if it no longer feeds an aggregator", name, job.If, tapSkip)
		case heavyAdvisoryJobs[name] && !sameExpression(job.If, heavyAdvisoryIf):
			t.Errorf("job %q is a heavy advisory job and carries `if:` %q, want exactly %q — out of every light run, the merge queue and an unlabelled pull request, like the heavy legs", name, job.If, heavyAdvisoryIf)
		case heavyAdvisoryJobs[name]:
			// The heavy literal keeps it out of the merge queue on its own.
		case name == heavyGateJob && !sameExpression(job.If, heavyGateIf):
			t.Errorf("job %q decides the debounced push and carries `if:` %q, want exactly %q — it decides a push and nothing else, and never holds a merge-queue slot", name, job.If, heavyGateIf)
		case name == heavyGateJob:
			// Push only: it skips the merge queue by never running there.
		case heldChecks[name]:
			t.Errorf("job %q is a held check and carries `if:` %q with needs %v: a held aggregator carries exactly %q, a held job of its own work is a heavy advisory job", name, job.If, job.Needs, aggregatorIf)
		case !requiredChecks[name] && !gatedJobs[name] && !skips:
			t.Errorf("job %q is advisory and does NOT skip merge_group (if: %q) — it holds a queue slot for a verdict nobody can act on; add `if: github.event_name != 'merge_group'` verbatim, spaces included, or add it to the ruleset and to requiredChecks here", name, job.If)
		}
	}
	// A step-level continue-on-error is the job-level one in disguise: the
	// step fails, the job reports success, and both the needs-graph and the
	// aggregator's re-verification read that success. Refused on every job
	// that feeds a required verdict, like the job-level flag above.
	for name, job := range wf.Jobs {
		if !requiredChecks[name] && !heldChecks[name] && !gatedJobs[name] {
			continue
		}
		for _, st := range job.Steps {
			if st.ContinueOnError != nil {
				t.Errorf("job %q step %q carries continue-on-error (%v) — the key is refused whatever its value: set true, a failing step lets the job report success to its aggregator and to the ruleset. Nothing that feeds a required verdict may carry it, at the job or at the step", name, st.Name, st.ContinueOnError)
			}
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
	for name := range heldChecks {
		if _, ok := wf.Jobs[name]; !ok {
			t.Errorf("heldChecks names %q, which this workflow does not define — the ruleset could never name it back", name)
		}
		if requiredChecks[name] {
			t.Errorf("%q is both required and held: a held check is out of the ruleset by definition", name)
		}
	}
	for _, bucket := range []map[string]bool{heavyLegs, heavyAdvisoryJobs} {
		for name := range bucket {
			if _, ok := wf.Jobs[name]; !ok {
				t.Errorf("%q is listed as heavy but this workflow does not define it — the bucket drifted from the file", name)
			}
		}
	}
}

// TestEveryGatedJobIsNeededByARequiredAggregator closes the orphan-leg hole:
// a gated leg no required or held aggregator `needs:` is red on its own job
// only — no context the ruleset names, now or when a held check returns to
// it, would carry its verdict.
func TestEveryGatedJobIsNeededByARequiredAggregator(t *testing.T) {
	var wf workflowJobs
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	for name := range gatedJobs {
		needed := false
		for jobName, job := range wf.Jobs {
			if requiredChecks[jobName] || heldChecks[jobName] {
				for _, dep := range job.Needs {
					if dep == name {
						needed = true
					}
				}
			}
		}
		if !needed {
			t.Errorf("gated leg %q is not named in any required or held job's needs: — no context the ruleset names carries its verdict. Add it to its aggregator's needs:, or move it out of gatedJobs", name)
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
//  3. every leg's `${{ needs.<leg>.result }}` is read into the job's env byte
//     for byte (no expression around it can massage the value). What the
//     steps then DO with the value is not read here but RUN:
//     TestAggregatorsGoRedOnEveryRedLeg executes them — a spelling check
//     (does the step text mention the env key?) stayed green under eleven
//     rewrites of the step that let every leg outcome through.
func TestAggregatorsReVerifyEveryLeg(t *testing.T) {
	var wf workflowJobs
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	for name, job := range wf.Jobs {
		if len(aggregatedLegs(job.Needs)) == 0 {
			// No need, or heavyGateJob alone: a heavy job reading the
			// decider's output, not an aggregator.
			continue
		}
		if strings.TrimSpace(job.If) != aggregatorIf {
			t.Errorf("job %q needs %v but carries `if:` %q — with plain needs, a failed leg SKIPS this job and a skipped check reports SUCCESS. Carry exactly %s", name, job.Needs, job.If, aggregatorIf)
		}
		for _, leg := range job.Needs {
			if leg == heavyGateJob {
				// Needed for its output, which LIGHT_RUN reads; it is no
				// leg, and TestHeavyLayerRunsOnTheDebouncedPush holds it.
				continue
			}
			if !gatedJobs[leg] {
				t.Errorf("job %q needs %q, which is not a gated leg — an advisory member reports `skipped` inside the merge queue and the aggregator would re-verify it into red", name, leg)
			}
			// Byte for byte, not "contains": `${{ needs.x.result && 'success' }}`
			// contains the needle and always yields success — an expression
			// around the read can massage the value before the step sees it.
			needle := "${{ needs." + leg + ".result }}"
			read := false
			for _, v := range job.Env {
				if sameExpression(v, needle) {
					read = true
				}
			}
			if !read {
				t.Errorf("job %q needs %q but no env value is exactly %q — a leg whose result is read through any expression, or not at all, could report anything", name, leg, needle)
			}
		}
		// An aggregator over a heavy leg learns whether the run is light
		// from the one evaluated literal the legs' skip is built from; any
		// other key or spelling could forgive a `skipped` leg in the queue.
		needsHeavy := false
		for _, leg := range job.Needs {
			needsHeavy = needsHeavy || heavyLegs[leg]
		}
		got, carries := job.Env[lightRunEnv]
		switch {
		case needsHeavy && (!carries || !sameExpression(got, lightRunValue)):
			t.Errorf("job %q needs a heavy leg but its %s is %q, want exactly %q", name, lightRunEnv, got, lightRunValue)
		case !needsHeavy && carries:
			t.Errorf("job %q needs no heavy leg yet carries %s: nothing it aggregates may be forgiven a skip", name, lightRunEnv)
		}
		for key, value := range job.Env {
			if key != lightRunEnv && strings.Contains(value, "pull_request") {
				t.Errorf("job %q reads the event through %s=%q: only %s may tell its step about the pull request", name, key, value, lightRunEnv)
			}
		}
	}
}

// TestAggregatorsGoRedOnEveryRedLeg RUNS each aggregator's steps the way
// `shell: bash` runs them, instead of reading their spelling: green with every
// leg at success, red when any one leg reports anything else — failure,
// cancelled or skipped, each leg in turn. Reading loses here as it does for
// `if:`: a check that the step text mentions the env key stayed green while
// the step echoed the value without comparing it, exited 0, compared a second
// massaged env key, took a step-level `env:` that overrides the job's, ran
// under `shell: true {0}` or a `defaults.run.shell`, inverted the comparison,
// or dropped `set -e` (measured, eleven shapes). So the step's shape is closed
// — exactly `name` + `shell: bash` + a `run` reading no `${{ }}`, so the run
// here is the run there — only the exact `${{ needs.<leg>.result }}` reads
// and the exact lightRunValue reach its env, and the verdict is the
// script's own exit code under a bare shell (no profile, no rc, no host env,
// a throwaway HOME and directory).
//
// Each aggregator runs under every value its light-run key can take (#3683,
// #3691). `false` is what GitHub evaluates on push, schedule, dispatch and an
// opted-in pull request: there every non-success leg, a skipped heavy leg
// included, is red. `true` is a light run, a merge-queue build or an
// unlabelled pull request: a heavy leg may report `skipped` and nothing else
// — its failure or cancellation, and any other leg's skip, stay red. Any
// other value (empty, a spelling GitHub never produces) is read as not light.
// Which event renders which value is event_layers_test.go's to evaluate.
func TestAggregatorsGoRedOnEveryRedLeg(t *testing.T) {
	var wf struct{ Jobs map[string]map[string]any }
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	for name, job := range wf.Jobs {
		needs, _ := job["needs"].([]any)
		env, _ := job["env"].(map[string]any)
		steps, _ := job["steps"].([]any)
		// green runs the steps with legs[leg] = result for the named legs and
		// success for every other, and the light key at light.
		green := func(light string, results map[string]string) bool {
			vars := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
			for k, v := range env {
				if v == lightRunValue {
					vars = append(vars, k+"="+light)
				}
				for _, l := range needs {
					if v == fmt.Sprintf("${{ needs.%v.result }}", l) {
						result, ok := results[fmt.Sprint(l)]
						if !ok {
							result = "success"
						}
						vars = append(vars, k+"="+result)
					}
				}
			}
			for _, s := range steps {
				st, _ := s.(map[string]any)
				run, _ := st["run"].(string)
				if len(st) != 3 || st["name"] == nil || st["shell"] != "bash" || strings.Contains(run, "${{") {
					t.Fatalf("aggregator %q step %v: want exactly name + `shell: bash` + a run reading no ${{ }} — anything else changes what runs here versus there", name, st)
				}
				cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", run)
				if cmd.Dir, cmd.Env = t.TempDir(), vars; cmd.Run() != nil {
					return false
				}
			}
			return true
		}
		if len(aggregatedLegs(needs)) == 0 {
			continue
		}
		allHeavySkipped := map[string]string{}
		for _, leg := range needs {
			if heavyLegs[fmt.Sprint(leg)] {
				allHeavySkipped[fmt.Sprint(leg)] = "skipped"
			}
		}
		for _, light := range []string{"false", "true", "", "True"} {
			lightRun := light == "true"
			if !green(light, nil) {
				t.Errorf("aggregator %q is RED with every leg at success (%s=%q) — its re-verification refuses a healthy entry", name, lightRunEnv, light)
			}
			for _, leg := range needs {
				leg := fmt.Sprint(leg)
				if leg == heavyGateJob {
					continue
				}
				for _, r := range []string{"failure", "cancelled", "skipped"} {
					forgiven := lightRun && heavyLegs[leg] && r == "skipped"
					switch got := green(light, map[string]string{leg: r}); {
					case got && !forgiven:
						t.Errorf("aggregator %q stays GREEN when leg %s reports %s with %s=%q — a red or unexecuted leg would merge under a green required check", name, leg, r, lightRunEnv, light)
					case !got && forgiven:
						t.Errorf("aggregator %q is RED when heavy leg %s is skipped on a light run — the light run the leg skipped by design would never pass", name, leg)
					}
				}
			}
			if len(allHeavySkipped) > 0 {
				if got := green(light, allHeavySkipped); got != lightRun {
					t.Errorf("aggregator %q with every heavy leg skipped and %s=%q is green=%v, want %v", name, lightRunEnv, light, got, lightRun)
				}
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

package ciguard

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// tapUpdate is the predicate tests.yml's heavy jobs skip a Homebrew tap update
// on: the merge-queue entry's head commit message, or the pull request's
// title, starts with the prefix brew-tap-pr.sh gives both. The queue squashes
// under COMMIT_OR_PR_TITLE (the repository setting), so a single-commit tap
// pull request — every one brew-tap-pr.sh opens — lands under its commit
// subject and a multi-commit one under its title: the script uses the prefix
// for both. On a push both operands are empty and the predicate is false.
const tapUpdate = `startsWith(github.event.merge_group.head_commit.message || github.event.pull_request.title, 'chore(brew): update tap to ')`

// tapSkip is the only `if:` a required job may carry (see
// TestEveryJobPicksASideOfTheMergeQueue).
const tapSkip = "!" + tapUpdate

// tapVerifierJob hosts the check that makes tapSkip safe: required, and never
// carrying tapSkip itself.
const tapVerifierJob = "brand"

const (
	tapVerifierRun      = "./scripts/ci/tap-entry-check.sh"
	brewUpdateWorkflow  = "../../.github/workflows/brew-update.yml"
	brewUpdateJobIf     = "github.event_name == 'workflow_dispatch' || github.event.workflow_run.event == 'schedule'"
	brewUpdateJobName   = "update"
	checkoutActionsName = "actions/checkout@"
)

// sameExpression compares two workflow expressions byte for byte. YAML has
// already been parsed, so there is no folding left to forgive — and any
// tolerance would reach inside the quoted literal, where GitHub's startsWith
// sees every byte: two spaces, a tab or a no-break space there would make the
// skip and the verifier disagree with every guard green.
func sameExpression(a, b string) bool {
	return a == b
}

type tapStep struct {
	Uses             string            `yaml:"uses"`
	Run              string            `yaml:"run"`
	If               string            `yaml:"if"`
	ContinueOnError  any               `yaml:"continue-on-error"`
	Shell            string            `yaml:"shell"`
	WorkingDirectory string            `yaml:"working-directory"`
	With             map[string]any    `yaml:"with"`
	Env              map[string]string `yaml:"env"`
}

type tapWorkflow struct {
	Defaults any `yaml:"defaults"`
	Jobs     map[string]struct {
		If              string    `yaml:"if"`
		Needs           any       `yaml:"needs"`
		ContinueOnError any       `yaml:"continue-on-error"`
		Defaults        any       `yaml:"defaults"`
		Steps           []tapStep `yaml:"steps"`
	} `yaml:"jobs"`
}

// TestTheTapSkipIsVerifiedWhereNothingSkipsIt holds the half of the tap skip
// that makes it safe. A skipped job reports SUCCESS and a title is only a
// claim, so a code pull request titled like a tap update would merge on checks
// that never ran — unless a required job that nothing skips refuses it. That
// job is `brand`, the step is scripts/ci/tap-entry-check.sh, and each way of
// quietly disarming it is refused here:
//
//   - a condition on the job or the step, continue-on-error on either, or a
//     `shell:` / `working-directory:` / `defaults:` that changes what `run:`
//     executes (`shell: true {0}` runs /usr/bin/true): the verifier stops
//     deciding anything;
//   - a `needs:` on the job: a job whose dependency was skipped is skipped
//     too, and a skipped required check counts as passed — `needs: test`
//     would skip the verifier exactly when the tap skip fires;
//   - TAP_ONLY drifting from the skip predicate: the verifier would judge a
//     different set of entries than the one the heavy jobs skipped — and
//     startsWith ignores case, so a stricter re-match would wave entries
//     through;
//   - an input wired to the wrong event field, or a checkout too shallow to
//     reach the test merge's parents;
//   - a step between the checkout and the verifier (it could write
//     GITHUB_ENV or GITHUB_PATH — a BASH_ENV to source, a fake git first on
//     PATH), and a BASH_ENV from the workflow or the job: the step pins it
//     empty.
func TestTheTapSkipIsVerifiedWhereNothingSkipsIt(t *testing.T) {
	src, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", workflowPath, err)
	}
	var wf tapWorkflow
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPath, err)
	}

	if !requiredChecks[tapVerifierJob] {
		t.Fatalf("the tap verifier lives in %q, which requiredChecks does not list: a verdict from a job that is not required blocks nothing", tapVerifierJob)
	}
	job, ok := wf.Jobs[tapVerifierJob]
	if !ok {
		t.Fatalf("%s defines no %q job — the tap skip has lost its verifier", workflowPath, tapVerifierJob)
	}
	if strings.TrimSpace(job.If) != "" {
		t.Errorf("%q carries `if: %s` — the job that verifies the tap skip must run on every event", tapVerifierJob, job.If)
	}
	if job.Needs != nil {
		t.Errorf("%q carries `needs: %v` — a job whose dependency is skipped is skipped too, and reports success: a tap skip of that dependency would skip the verifier with it", tapVerifierJob, job.Needs)
	}
	if job.ContinueOnError != nil {
		t.Errorf("%q sets continue-on-error — a verifier whose failure is forgiven verifies nothing", tapVerifierJob)
	}
	if wf.Defaults != nil || job.Defaults != nil {
		t.Errorf("%s sets `defaults:` (workflow %v, %q job %v) — a default shell or directory changes what the verifier's `run:` executes", workflowPath, wf.Defaults, tapVerifierJob, job.Defaults)
	}

	checkout, verifier := -1, -1
	for i, s := range job.Steps {
		if strings.HasPrefix(s.Uses, checkoutActionsName) && checkout < 0 {
			checkout = i
		}
		if strings.TrimSpace(s.Run) == tapVerifierRun {
			if verifier >= 0 {
				t.Errorf("%q runs %s twice", tapVerifierJob, tapVerifierRun)
			}
			verifier = i
		}
	}
	if verifier < 0 {
		t.Fatalf("%q has no step running %s — every tap update would merge on the strength of its title alone", tapVerifierJob, tapVerifierRun)
	}
	if checkout != 0 || verifier != 1 {
		t.Fatalf("%q must check out the repository as its first step and run %s right after it (checkout step %d, verifier step %d) — a step between them could rewrite the verifier's environment", tapVerifierJob, tapVerifierRun, checkout, verifier)
	}
	if depth := fmt.Sprint(job.Steps[checkout].With["fetch-depth"]); depth != "2" {
		t.Errorf("%q checks out with fetch-depth %s, want 2: the verifier diffs a pull request's test merge against its first parent", tapVerifierJob, depth)
	}

	step := job.Steps[verifier]
	if strings.TrimSpace(step.If) != "" {
		t.Errorf("the verifier step carries `if: %s` — it must run whenever its job does", step.If)
	}
	if step.ContinueOnError != nil {
		t.Errorf("the verifier step sets continue-on-error — its failure must fail %q", tapVerifierJob)
	}
	if step.Shell != "" || step.WorkingDirectory != "" {
		t.Errorf("the verifier step sets shell %q / working-directory %q — either changes what its `run:` executes", step.Shell, step.WorkingDirectory)
	}
	want := map[string]string{
		"TAP_ONLY":       "${{ " + tapUpdate + " }}",
		"EVENT_NAME":     "${{ github.event_name }}",
		"QUEUE_BASE_SHA": "${{ github.event.merge_group.base_sha }}",
		"QUEUE_HEAD_SHA": "${{ github.event.merge_group.head_sha }}",
		"QUEUE_BASE_REF": "${{ github.event.merge_group.base_ref }}",
		"PR_HEAD_SHA":    "${{ github.event.pull_request.head.sha }}",
		"GH_TOKEN":       "${{ github.token }}",
		"BASH_ENV":       "",
	}
	for k, v := range want {
		if got, ok := step.Env[k]; !ok || !sameExpression(got, v) {
			t.Errorf("the verifier step's %s is %q, want %q", k, got, v)
		}
	}
	for k := range step.Env {
		if _, ok := want[k]; !ok {
			t.Errorf("the verifier step sets %s, which this guard does not know — add it to the expected inputs or drop it", k)
		}
	}
}

// TestTheTapPullRequestCarriesTheSkipTitle holds the other end of the
// predicate: the pull request's title, read on the pull request, and its
// commit subject, read in the queue (a single-commit pull request squashes
// under its commit subject here). Renaming either alone fails safe — the heavy
// jobs run again — but it silently ends what the skip exists for, and nobody
// reads a queue's latency until it hurts.
func TestTheTapPullRequestCarriesTheSkipTitle(t *testing.T) {
	m := regexp.MustCompile(`'([^']+)'\)$`).FindStringSubmatch(tapUpdate)
	if m == nil {
		t.Fatalf("cannot read the title prefix out of tapUpdate %q", tapUpdate)
	}
	prefix := m[1]
	src, err := os.ReadFile(brewTapScript)
	if err != nil {
		t.Fatalf("read %s: %v", brewTapScript, err)
	}
	for _, carrier := range []struct{ what, pattern string }{
		{"the pull request title (`gh pr create --title`)", `--title "([^"]*)"`},
		{"the commit subject (`git commit -m`)", `git commit -m "([^"]*)"`},
	} {
		found := regexp.MustCompile(carrier.pattern).FindAllStringSubmatch(string(src), -1)
		if len(found) == 0 {
			t.Errorf("%s sets no %s — this guard can no longer see it", brewTapScript, carrier.what)
		}
		for _, m := range found {
			if !strings.HasPrefix(m[1], prefix) {
				t.Errorf("%s sets %s to %q, which does not start with %q — tests.yml's heavy jobs would run on every tap update", brewTapScript, carrier.what, m[1], prefix)
			}
		}
	}
}

// TestTheTapUpdateFollowsTheNightlyRelease holds brew-update.yml's trigger: the
// completion of version.yml's nightly run. A `workflow_run` naming a workflow
// that does not exist never fires and reports nothing — and the name carries an
// emoji with a variation selector, so one retyped byte is enough. Without the
// schedule filter it would fire once per merged pull request instead of once a
// night.
func TestTheTapUpdateFollowsTheNightlyRelease(t *testing.T) {
	var version struct {
		Name string `yaml:"name"`
	}
	src, err := os.ReadFile(releaseWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", releaseWorkflowPath, err)
	}
	if err := yaml.Unmarshal(src, &version); err != nil {
		t.Fatalf("parse %s: %v", releaseWorkflowPath, err)
	}

	var brew struct {
		On struct {
			WorkflowRun struct {
				Workflows []string `yaml:"workflows"`
				Types     []string `yaml:"types"`
			} `yaml:"workflow_run"`
		} `yaml:"on"`
		Jobs map[string]struct {
			If string `yaml:"if"`
		} `yaml:"jobs"`
	}
	src, err = os.ReadFile(brewUpdateWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", brewUpdateWorkflow, err)
	}
	if err := yaml.Unmarshal(src, &brew); err != nil {
		t.Fatalf("parse %s: %v", brewUpdateWorkflow, err)
	}

	if got := brew.On.WorkflowRun.Workflows; len(got) != 1 || got[0] != version.Name {
		t.Errorf("brew-update.yml follows workflow_run %q, want exactly [%q] — version.yml's name, byte for byte", got, version.Name)
	}
	if got := brew.On.WorkflowRun.Types; len(got) != 1 || got[0] != "completed" {
		t.Errorf("brew-update.yml's workflow_run types are %q, want [completed]", got)
	}
	job, ok := brew.Jobs[brewUpdateJobName]
	if !ok {
		t.Fatalf("brew-update.yml defines no %q job", brewUpdateJobName)
	}
	if !sameExpression(job.If, brewUpdateJobIf) {
		t.Errorf("brew-update.yml's %q job runs on `if: %s`, want `if: %s` — a dispatch, or version.yml's nightly and nothing else", brewUpdateJobName, job.If, brewUpdateJobIf)
	}
}

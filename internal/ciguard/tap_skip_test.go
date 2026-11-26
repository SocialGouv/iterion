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

// tapSkip is the only `if:` a required job that does its own work may carry,
// and the tap half of aggregatorIf, the only `if:` a required aggregator may
// carry (see TestEveryJobPicksASideOfTheMergeQueue).
const tapSkip = "!" + tapUpdate

// tapVerifierJob hosts the check that makes tapSkip safe: required, and never
// carrying tapSkip itself.
const tapVerifierJob = "brand"

const (
	tapVerifierRun      = "./scripts/ci/tap-entry-check.sh"
	brewUpdateWorkflow  = "../../.github/workflows/brew-update.yml"
	brewUpdateJobIf     = "github.event_name == 'workflow_dispatch' || github.event.workflow_run.conclusion == 'success'"
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
	src, err := os.ReadFile(workflowPaths[0])
	if err != nil {
		t.Fatalf("read %s: %v", workflowPaths[0], err)
	}
	var wf tapWorkflow
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}

	if !requiredChecks[tapVerifierJob] {
		t.Fatalf("the tap verifier lives in %q, which requiredChecks does not list: a verdict from a job that is not required blocks nothing", tapVerifierJob)
	}
	job, ok := wf.Jobs[tapVerifierJob]
	if !ok {
		t.Fatalf("%s defines no %q job — the tap skip has lost its verifier", workflowPaths[0], tapVerifierJob)
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
		t.Errorf("%s sets `defaults:` (workflow %v, %q job %v) — a default shell or directory changes what the verifier's `run:` executes", workflowPaths[0], wf.Defaults, tapVerifierJob, job.Defaults)
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

// binariesWorkflowPath is release.yml: run from a v* tag, it attaches the CLI
// binaries the tap formula needs to that tag's GitHub release.
const binariesWorkflowPath = "../../.github/workflows/release.yml"

// desktopWorkflowPath is desktop-release.yml: it attaches a tag's desktop ZIP,
// the cask's artefact, to that tag's GitHub release.
const desktopWorkflowPath = "../../.github/workflows/desktop-release.yml"

// TestTheTapUpdateFollowsTheReleaseBinaries holds brew-update.yml's trigger:
// the completion of 📦 Release (release.yml), the run that attaches a tag's CLI
// binaries, and of 🖥️ Desktop Release (desktop-release.yml), the run that
// attaches its desktop ZIP, and only when it concluded success — the moment
// the tag, or its cask, can be tapped. A `workflow_run` naming a workflow that does not exist never fires
// and reports nothing, and the name carries an emoji with a variation
// selector, so one retyped byte is enough. A dispatch with a `tag` input stays
// the way to tap one release now. Which tag a run then taps — one whose
// release holds the four binaries — is brew-tap-resolve.sh's, held by
// tap_resolve_test.go.
func TestTheTapUpdateFollowsTheReleaseBinaries(t *testing.T) {
	var binaries struct {
		Name string `yaml:"name"`
		On   struct {
			Push struct {
				Tags []string `yaml:"tags"`
			} `yaml:"push"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(mustRead(t, binariesWorkflowPath), &binaries); err != nil {
		t.Fatalf("parse %s: %v", binariesWorkflowPath, err)
	}
	if got := binaries.On.Push.Tags; len(got) != 1 || got[0] != "v*" {
		t.Errorf("%s runs on push tags %q, want [v*] — the tap follows this workflow because each of its runs is one release tag's binaries", binariesWorkflowPath, got)
	}

	var brew struct {
		On struct {
			WorkflowRun struct {
				Workflows []string `yaml:"workflows"`
				Types     []string `yaml:"types"`
			} `yaml:"workflow_run"`
			WorkflowDispatch struct {
				Inputs map[string]any `yaml:"inputs"`
			} `yaml:"workflow_dispatch"`
		} `yaml:"on"`
		Jobs map[string]struct {
			If string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(mustRead(t, brewUpdateWorkflow), &brew); err != nil {
		t.Fatalf("parse %s: %v", brewUpdateWorkflow, err)
	}

	var desktop struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(mustRead(t, desktopWorkflowPath), &desktop); err != nil {
		t.Fatalf("parse %s: %v", desktopWorkflowPath, err)
	}
	if got := brew.On.WorkflowRun.Workflows; len(got) != 2 || got[0] != binaries.Name || got[1] != desktop.Name {
		t.Errorf("brew-update.yml follows workflow_run %q, want exactly [%q %q] — release.yml's and desktop-release.yml's names, byte for byte: the cask catches up with a desktop ZIP only on a run Desktop Release starts", got, binaries.Name, desktop.Name)
	}
	if got := brew.On.WorkflowRun.Types; len(got) != 1 || got[0] != "completed" {
		t.Errorf("brew-update.yml's workflow_run types are %q, want [completed]", got)
	}
	if _, ok := brew.On.WorkflowDispatch.Inputs["tag"]; !ok {
		t.Errorf("brew-update.yml's workflow_dispatch lost its `tag` input — the way to tap one release now")
	}
	job, ok := brew.Jobs[brewUpdateJobName]
	if !ok {
		t.Fatalf("brew-update.yml defines no %q job", brewUpdateJobName)
	}
	if !sameExpression(job.If, brewUpdateJobIf) {
		t.Errorf("brew-update.yml's %q job runs on `if: %s`, want `if: %s` — a dispatch, or a Release run that succeeded and nothing else: a red one may lack the binaries", brewUpdateJobName, job.If, brewUpdateJobIf)
	}
}

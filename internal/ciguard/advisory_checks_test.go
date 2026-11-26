package ciguard

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// advisoryChecksJob is the one tests.yml job that carries several advisory
// checks in one pod (#3711): the desktop vet under GOOS=windows and
// GOOS=darwin, the chart lint and render gate, govulncheck and the
// documentation site build.
const advisoryChecksJob = "advisory-checks"

// everyStepRunsIf is the condition every step of advisoryChecksJob carries.
// A step's implicit condition is `success()`: without this one, the first
// red check would skip every check after it, and the job would report one
// verdict where it owes five.
const everyStepRunsIf = "${{ !cancelled() }}"

// advisoryCheckSteps are the named steps that carry the merged checks' work,
// one entry per check step of the jobs it merged. A step dropped in an edit of the merged job
// loses a check without any job going red; this list is what notices.
var advisoryCheckSteps = []string{
	"Vet the desktop package (windows)",
	"Vet the desktop package (darwin)",
	"helm dependency update",
	"helm lint (values-dev.yaml)",
	"helm lint --strict (values-prod.yaml)",
	"image.digest renders and rejects a malformed value",
	"Install yq (pinned)",
	"chart render tests (dead selectors, backup and alert guards)",
	"Install govulncheck",
	"Run govulncheck (SARIF)",
	"Deduplicate SARIF stacks",
	"Upload SARIF",
	"Link checker self-test",
	"Install dependencies",
	"Build VitePress site",
}

// advisoryTimedSteps are the `run:` steps that download a tool or
// dependencies, and the long checks (chart render tests, govulncheck, the
// site build), each with a timeout of its own; every other step relies on
// the job budget alone, and the test holds the list to the steps that carry
// one. The job's timeout cancels the job, and a cancelled job skips every
// `!cancelled()` step after it: a step hung against the job budget would
// silence the checks that follow. The values are tuning, so the test asks
// only that each exists and fits the job's.
var advisoryTimedSteps = []string{
	"helm dependency update",
	"Install yq (pinned)",
	"chart render tests (dead selectors, backup and alert guards)",
	"Install govulncheck",
	"Run govulncheck (SARIF)",
	"Install dependencies",
	"Build VitePress site",
}

type advisoryChecksWorkflow struct {
	Jobs map[string]struct {
		TimeoutMinutes  int               `yaml:"timeout-minutes"`
		ContinueOnError any               `yaml:"continue-on-error"`
		Permissions     map[string]string `yaml:"permissions"`
		Steps           []struct {
			Name            string            `yaml:"name"`
			TimeoutMinutes  int               `yaml:"timeout-minutes"`
			Run             string            `yaml:"run"`
			Uses            string            `yaml:"uses"`
			If              string            `yaml:"if"`
			Env             map[string]string `yaml:"env"`
			With            map[string]any    `yaml:"with"`
			ContinueOnError any               `yaml:"continue-on-error"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// TestTheAdvisoryChecksJobRunsEveryCheck holds the contract that lets five
// checks share one job without losing a verdict:
//
//   - every step carries exactly everyStepRunsIf, so a failing check does
//     not skip the ones after it, and the red step names the failed check;
//   - no continue-on-error, on the job or a step: the job is red when any
//     check is red, which is the only signal an advisory check gives;
//   - every check step is present, and the two desktop vets set their GOOS;
//   - the checkout persists no token: the job holds `security-events: write`
//     for the SARIF upload, and its other steps run downloaded code;
//   - every action from outside GitHub is passed an empty token.
func TestTheAdvisoryChecksJobRunsEveryCheck(t *testing.T) {
	src, err := os.ReadFile(workflowPaths[0])
	if err != nil {
		t.Fatalf("read %s: %v", workflowPaths[0], err)
	}
	var wf advisoryChecksWorkflow
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	job, ok := wf.Jobs[advisoryChecksJob]
	if !ok {
		t.Fatalf("%s defines no job %q", workflowPaths[0], advisoryChecksJob)
	}
	if len(job.Steps) == 0 {
		t.Fatalf("job %q has no step", advisoryChecksJob)
	}
	if job.ContinueOnError != nil {
		t.Errorf("job %q carries continue-on-error (%v): a red check would report green", advisoryChecksJob, job.ContinueOnError)
	}

	var names []string
	for i, st := range job.Steps {
		label := st.Name
		if label == "" {
			label = st.Uses
		}
		if st.If != everyStepRunsIf {
			t.Errorf("job %q step %d (%s) carries `if: %s`, want exactly %q: any other condition lets an earlier red step skip this check",
				advisoryChecksJob, i, label, st.If, everyStepRunsIf)
		}
		if st.ContinueOnError != nil {
			t.Errorf("job %q step %d (%s) carries continue-on-error (%v): its red would not reach the job", advisoryChecksJob, i, label, st.ContinueOnError)
		}
		if st.Name != "" {
			names = append(names, st.Name)
		}
	}
	for _, want := range advisoryCheckSteps {
		if !slices.Contains(names, want) {
			t.Errorf("job %q has no step %q: a check left the merged job and nothing else runs it", advisoryChecksJob, want)
		}
	}
	for _, goos := range []string{"windows", "darwin"} {
		name := "Vet the desktop package (" + goos + ")"
		for _, st := range job.Steps {
			if st.Name != name {
				continue
			}
			if st.Env["GOOS"] != goos || st.Env["CGO_ENABLED"] != "0" {
				t.Errorf("job %q step %q sets env %v, want GOOS=%s and CGO_ENABLED=0", advisoryChecksJob, name, st.Env, goos)
			}
		}
	}

	for _, st := range job.Steps {
		if !slices.Contains(advisoryTimedSteps, st.Name) {
			if st.TimeoutMinutes != 0 {
				label := st.Name
				if label == "" {
					label = st.Uses
				}
				t.Errorf("job %q step %q carries timeout-minutes %d but is not in advisoryTimedSteps: the list, and the job comment it mirrors, no longer name every timed step",
					advisoryChecksJob, label, st.TimeoutMinutes)
			}
			continue
		}
		if st.TimeoutMinutes <= 0 || st.TimeoutMinutes >= job.TimeoutMinutes {
			t.Errorf("job %q step %q has timeout-minutes %d, want a step timeout below the job's %d: a hang would use up the job budget and the job's cancellation skips every check after it",
				advisoryChecksJob, st.Name, st.TimeoutMinutes, job.TimeoutMinutes)
		}
	}
	for _, st := range job.Steps {
		if st.Name != "Upload SARIF" {
			continue
		}
		if st.With["sarif_file"] != "govulncheck.sarif" || st.With["category"] != "govulncheck" {
			t.Errorf("job %q step %q uploads with %v, want sarif_file=govulncheck.sarif and category=govulncheck: the upload is govulncheck's only output",
				advisoryChecksJob, st.Name, st.With)
		}
	}

	if job.Permissions["security-events"] == "write" {
		checkout := job.Steps[0]
		if action, _, _ := strings.Cut(checkout.Uses, "@"); action != "actions/checkout" {
			t.Fatalf("job %q starts with %q, want actions/checkout", advisoryChecksJob, checkout.Uses)
		}
		if v, ok := checkout.With["persist-credentials"].(bool); !ok || v {
			t.Errorf("job %q holds security-events: write and its checkout persists the token (persist-credentials: %v): the govulncheck run and the VitePress build would find it in .git/config",
				advisoryChecksJob, checkout.With["persist-credentials"])
		}
		for _, st := range job.Steps {
			owner, _, _ := strings.Cut(st.Uses, "/")
			if st.Uses == "" || owner == "actions" || owner == "github" {
				continue
			}
			if v, ok := st.With["token"].(string); !ok || v != "" {
				t.Errorf("job %q holds security-events: write and step %q, an action from outside GitHub, receives the job token (with.token: %v): pass `token: ''`",
					advisoryChecksJob, st.Uses, st.With["token"])
			}
		}
	}
}

// TestTheGovulncheckRunStepFailsWithoutTheTool runs the job's own
// `Run govulncheck (SARIF)` script. Every step of advisoryChecksJob runs
// after a failed one, so a failed install still reaches this step: without
// the binary it must fail and leave no govulncheck.sarif, or the upload
// would publish an empty analysis that reads as "govulncheck found nothing".
// The control puts a stub govulncheck on PATH and expects a SARIF file.
func TestTheGovulncheckRunStepFailsWithoutTheTool(t *testing.T) {
	src, err := os.ReadFile(workflowPaths[0])
	if err != nil {
		t.Fatalf("read %s: %v", workflowPaths[0], err)
	}
	var wf advisoryChecksWorkflow
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	var script string
	for _, st := range wf.Jobs[advisoryChecksJob].Steps {
		if st.Name == "Run govulncheck (SARIF)" {
			script = st.Run
		}
	}
	if script == "" {
		t.Fatalf("job %q has no `Run govulncheck (SARIF)` step with a script", advisoryChecksJob)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH: %v", err)
	}

	// run executes the script the way a GitHub `run:` does (bash -e) in a
	// fresh directory, with PATH reduced to binDir.
	run := func(binDir string) (string, error) {
		dir := t.TempDir()
		cmd := exec.Command(bash, "-e", "-c", script)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + binDir, "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir()}
		out, err := cmd.CombinedOutput()
		if _, statErr := os.Stat(filepath.Join(dir, "govulncheck.sarif")); statErr == nil {
			return string(out), errSarifWritten
		}
		return string(out), err
	}

	out, err := run(t.TempDir())
	if err == nil {
		t.Errorf("the run step exits 0 with no govulncheck on PATH; output:\n%s", out)
	}
	if errors.Is(err, errSarifWritten) {
		t.Errorf("the run step writes govulncheck.sarif with no govulncheck on PATH, so the upload would publish an empty analysis; output:\n%s", out)
	}

	stubDir := t.TempDir()
	stub := "#!/bin/sh\nprintf '%s' '{\"version\":\"2.1.0\",\"runs\":[]}'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "govulncheck"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub govulncheck: %v", err)
	}
	if out, err := run(stubDir); !errors.Is(err, errSarifWritten) {
		t.Errorf("with a govulncheck on PATH the run step returns %v and no govulncheck.sarif; output:\n%s", err, out)
	}
}

// errSarifWritten marks a run of the govulncheck script that left
// govulncheck.sarif behind.
var errSarifWritten = errors.New("govulncheck.sarif written")

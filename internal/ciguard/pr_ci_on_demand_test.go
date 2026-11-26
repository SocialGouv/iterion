package ciguard

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestTheAggregatorsReadThePolicy RUNS each aggregator's step under every
// value prCIEnv can carry, the legs at what such a run gives them: `refused`
// is red with every leg skipped AND with every leg green — the policy, not a
// leg, decides — and says the policy and both ways out; `run` is green with
// every leg green; any value GitHub never renders is red.
// event_layers_test.go renders which event gives which value.
func TestTheAggregatorsReadThePolicy(t *testing.T) {
	var wf struct{ Jobs map[string]map[string]any }
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	aggregators := 0
	for name, job := range wf.Jobs {
		env, _ := job["env"].(map[string]any)
		if env[prCIEnv] != prCIValue {
			continue
		}
		aggregators++
		needs, _ := job["needs"].([]any)
		run := func(policy, legs string) (bool, string) {
			vars := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), prCIEnv + "=" + policy, lightRunEnv + "=true"}
			for k, v := range env {
				for _, l := range needs {
					if v == fmt.Sprintf("${{ needs.%v.result }}", l) {
						vars = append(vars, k+"="+legs)
					}
				}
			}
			var out strings.Builder
			for _, s := range job["steps"].([]any) {
				st, _ := s.(map[string]any)
				body, _ := st["run"].(string)
				cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", body)
				cmd.Dir, cmd.Env = t.TempDir(), vars
				b, err := cmd.CombinedOutput()
				out.Write(b)
				if err != nil {
					return false, out.String()
				}
			}
			return true, out.String()
		}
		for _, legs := range []string{"skipped", "success"} {
			green, out := run("refused", legs)
			if green {
				t.Errorf("aggregator %q is GREEN on a refused pull request with every leg %s — a required check over nothing", name, legs)
			}
			for _, word := range policyWords {
				if !strings.Contains(out, word) {
					t.Errorf("aggregator %q refuses without saying %s:\n%s", name, word, out)
				}
			}
		}
		for _, policy := range []string{"", "Run", "RUN", "bogus", "mirror", "run "} {
			if green, _ := run(policy, "success"); green {
				t.Errorf("aggregator %q is GREEN with %s=%q, a value GitHub never renders — red rather than a guess", name, prCIEnv, policy)
			}
		}
		if green, out := run("run", "success"); !green {
			t.Errorf("aggregator %q is RED on a run whose legs all succeeded:\n%s", name, out)
		}
	}
	if aggregators != 2 {
		t.Fatalf("found %d aggregators reading %s, want test and race", aggregators, prCIEnv)
	}
}

// TestNoSkippedRequiredCheckPrecedesTheRefusal holds the ORDER in which a
// refused run creates its required check runs (#3738). A job its `if:` skips
// reports SUCCESS the moment GitHub creates it, and GitHub creates a job
// without `needs:` in the run's first instant, a job with `needs:` only once
// every job it needs concluded (measured on runs 45753841731, 45868894520,
// 45873933090: skipped jobs at the run's first second, `race` one second
// after test-native-exec completed, `test` one second after its last leg).
// On a pull request that did not ask for CI, `test` refuses; its legs all
// skip in the first instant, so it is created right after them. A required
// job skipped by the policy, created before `test`, would for that gap be the
// newest check run of its name, green over the red run before it, with no
// refusal yet. So every required job the policy skips — every one but the
// tap verifier and the aggregators — needs tapVerifierJob: brand carries no
// `if:` and no `needs:` (TestTheTapSkipIsVerifiedWhereNothingSkipsIt), so its
// check run is created pending in the run's first instant and concludes only
// after its steps ran on a runner; a job behind it is created after `test`,
// and while it does not exist the newest `brand` is pending, which blocks the
// merge on its own. Its `if:` starts with `!cancelled()`, so a red brand does
// not skip it (event_layers_test.go evaluates that through jobRunner).
func TestNoSkippedRequiredCheckPrecedesTheRefusal(t *testing.T) {
	var wf workflowJobs
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	refused := pullRequest("synchronize", "", nil)
	checked := 0
	for name := range requiredChecks {
		job, ok := wf.Jobs[name]
		if !ok || name == tapVerifierJob || len(aggregatedLegs(job.Needs)) > 0 {
			continue
		}
		v, err := evalContexts(job.If, refused, gateNeeds("skipped", ""))
		if err != nil {
			t.Fatalf("job %q `if:` %q: %v", name, job.If, err)
		}
		if truthy(v) {
			continue
		}
		checked++
		if !slices.Equal(job.Needs, []string{tapVerifierJob}) {
			t.Errorf("required job %q skips on a refused pull request and needs %v, want exactly [%s]: created in the run's first instant, its skipped — green — check run would be the newest of its name before `test` refuses", name, job.Needs, tapVerifierJob)
		}
		if !strings.HasPrefix(job.If, "${{ !cancelled() && ") {
			t.Errorf("required job %q needs %s but its `if:` %q does not start with `!cancelled() && `: a red brand would skip it, green, on every event", name, tapVerifierJob, job.If)
		}
	}
	if checked < 3 {
		t.Fatalf("found %d required jobs a refused pull request skips, want vendor-check, golangci and fmt-check at least", checked)
	}
	brand := wf.Jobs[tapVerifierJob]
	if len(brand.Needs) != 0 || strings.TrimSpace(brand.If) != "" {
		t.Errorf("%s carries needs %v / if %q: its check run must be created pending in the run's first instant", tapVerifierJob, brand.Needs, brand.If)
	}

	// The order above holds while `test` is created in the refused run's
	// first instant: every job it and `race` need, transitively, skips there
	// at once, needing nothing but heavyGateJob, which skips every pull
	// request. A leg that gained a need on a job that runs (brand, a
	// setup job) would put `test` behind a runner, after the skipped
	// required jobs.
	if v, err := evalContexts(wf.Jobs[heavyGateJob].If, refused, nil); err != nil || truthy(v) {
		t.Errorf("%s `if:` %q runs on a pull request (%v): the legs that need it would wait for a runner", heavyGateJob, wf.Jobs[heavyGateJob].If, err)
	}
	seen := map[string]bool{}
	queue := []string{"test", "race"}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for _, need := range wf.Jobs[name].Needs {
			if seen[need] {
				continue
			}
			seen[need] = true
			queue = append(queue, need)
			for _, n := range wf.Jobs[need].Needs {
				if n != heavyGateJob {
					t.Errorf("%q, which `test` or `race` needs, needs %q: only %s, which skips every pull request, may sit under an aggregator, or `test` is no longer created in a refused run's first instant", need, n, heavyGateJob)
				}
			}
		}
	}
	if len(seen) < 5 {
		t.Fatalf("`test` and `race` need %d jobs transitively, want the legs and %s at least", len(seen), heavyGateJob)
	}
}

package ciguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// labelVerdictScript is the reader labelVerdictJob runs, relative to the repo
// root (the job's step) and to this package (the tests below).
const (
	labelVerdictScript     = "scripts/ci/label-verdict.sh"
	labelVerdictScriptPath = "../../" + labelVerdictScript
)

// TestALabelThatAsksForNoCIRepeatsTheVerdict holds labelVerdictJob's wiring
// (#3738): it runs on an eligible pull request's ignored label alone, on a
// GitHub-hosted runner with `actions: read` and a short ceiling, runs the
// shipped script with the pull request's head commit and the label, and
// exports the script's `test` and `race` outputs, which each aggregator reads
// (TestAggregatorsReVerifyEveryLeg pins that end). What each output does per
// event is event_layers_test.go's to evaluate.
func TestALabelThatAsksForNoCIRepeatsTheVerdict(t *testing.T) {
	var wf struct {
		Jobs map[string]struct {
			If          string            `yaml:"if"`
			RunsOn      any               `yaml:"runs-on"`
			Timeout     int               `yaml:"timeout-minutes"`
			Permissions map[string]string `yaml:"permissions"`
			Outputs     map[string]string `yaml:"outputs"`
			Needs       []string          `yaml:"needs"`
			Steps       []struct {
				ID   string            `yaml:"id"`
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				With map[string]any    `yaml:"with"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	job, ok := wf.Jobs[labelVerdictJob]
	if !ok {
		t.Fatalf("%s has no %q job: a run an unrelated label starts would write skipped — green — required checks over the verdict before it", workflowPaths[0], labelVerdictJob)
	}
	if !sameExpression(job.If, labelVerdictIf) {
		t.Errorf("%q `if:` is %q, want exactly %q", labelVerdictJob, job.If, labelVerdictIf)
	}
	if job.RunsOn != "ubuntu-latest" {
		t.Errorf("%q runs on %v, want ubuntu-latest: an API read needs no scale-set pod", labelVerdictJob, job.RunsOn)
	}
	if job.Timeout < 1 || job.Timeout > 10 {
		t.Errorf("%q timeout-minutes is %d, want 1..10", labelVerdictJob, job.Timeout)
	}
	if job.Permissions["actions"] != "read" {
		t.Errorf("%q permissions %v lack `actions: read`: the history it repeats would be unreadable", labelVerdictJob, job.Permissions)
	}
	if len(job.Needs) != 0 {
		t.Errorf("%q needs %v: it reads a finished run and waits on nothing", labelVerdictJob, job.Needs)
	}
	for _, out := range []string{"test", "race"} {
		if got, want := job.Outputs[out], "${{ steps.verdict.outputs."+out+" }}"; got != want {
			t.Errorf("%q output %s is %q, want %q", labelVerdictJob, out, got, want)
		}
	}
	found := false
	for _, st := range job.Steps {
		if st.ID != "verdict" {
			continue
		}
		found = true
		if strings.TrimSpace(st.Run) != labelVerdictScript {
			t.Errorf("%q step verdict runs %q, want exactly %q: the script the tests below run is the script CI runs", labelVerdictJob, st.Run, labelVerdictScript)
		}
		want := map[string]string{
			"GH_TOKEN": "${{ github.token }}",
			"HEAD_SHA": "${{ github.event.pull_request.head.sha }}",
			"LABEL":    "${{ github.event.label.name }}",
		}
		for k, v := range want {
			if st.Env[k] != v {
				t.Errorf("%q step verdict passes %s=%q, want %q", labelVerdictJob, k, st.Env[k], v)
			}
		}
	}
	if !found {
		t.Errorf("%q has no step with id verdict", labelVerdictJob)
	}

	// The script repeats the required contexts this run skips: every
	// required check but the tap verifier, which runs again in this run —
	// and `race`, the held aggregator, on its own output.
	src := string(mustRead(t, labelVerdictScriptPath))
	m := regexp.MustCompile(`(?m)verdict ((?:[a-z-]+ ?)+) <<<"\$jobs_json"`).FindAllStringSubmatch(src, -1)
	if len(m) != 2 {
		t.Fatalf("%s calls verdict %d times on the jobs, want twice (test, race)", labelVerdictScript, len(m))
	}
	var wantTest []string
	for name := range requiredChecks {
		if name != tapVerifierJob {
			wantTest = append(wantTest, name)
		}
	}
	sort.Strings(wantTest)
	gotTest := strings.Fields(m[0][1])
	sort.Strings(gotTest)
	if !slices.Equal(gotTest, wantTest) {
		t.Errorf("%s repeats %v for `test`, want every required check but %s: %v — a required context it forgets would be skipped green over a red one", labelVerdictScript, gotTest, tapVerifierJob, wantTest)
	}
	if got := strings.Fields(m[1][1]); !slices.Equal(got, []string{"race"}) || !heldChecks["race"] {
		t.Errorf("%s repeats %v for `race`, want [race], the held aggregator", labelVerdictScript, got)
	}
}

// labelRun is one canned tests.yml run on the pull request's head commit.
type labelRun struct {
	created, status, conclusion string
	jobs                        map[string]string
}

// greenJobs is every job a run the label repeats is read for, each green.
func greenJobs() map[string]string {
	return map[string]string{"test": "success", "vendor-check": "success", "golangci": "success", "fmt-check": "success", "brand": "success", "race": "success", "test-unit (0)": "success"}
}

func labelRunWith(created, conclusion string, edit map[string]string) labelRun {
	jobs := greenJobs()
	for k, v := range edit {
		if v == "" {
			delete(jobs, k)
			continue
		}
		jobs[k] = v
	}
	return labelRun{created: created, status: "completed", conclusion: conclusion, jobs: jobs}
}

// labelSelf is this run's id and creation time in every canned history.
const (
	labelSelf        = 500
	labelSelfCreated = "2026-11-26T12:00:00Z"
)

// runLabelVerdict runs the shipped script with a stand-in gh serving runs
// (this run, labelSelf, is added) and returns its exit code, the outputs it
// wrote and its log. broken and garbled name an API path fragment the
// stand-in fails on or answers with non-JSON.
func runLabelVerdict(t *testing.T, runs map[int]labelRun, broken, garbled string) (code int, outputs, log string) {
	t.Helper()
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Fatalf("jq is required by %s; it is declared in devbox.json, so run through `devbox run --`: %v", labelVerdictScript, err)
	}
	script, err := filepath.Abs(scriptUnderTest(t, labelVerdictScriptPath))
	if err != nil {
		t.Fatal(err)
	}
	dir, docs := t.TempDir(), t.TempDir()
	all := map[int]labelRun{labelSelf: {created: labelSelfCreated, status: "in_progress", jobs: map[string]string{}}}
	for id, r := range runs {
		all[id] = r
	}
	ids := make([]int, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var listed []string
	for _, id := range ids {
		r := all[id]
		conclusion := "null"
		if r.conclusion != "" {
			conclusion = fmt.Sprintf("%q", r.conclusion)
		}
		listed = append(listed, fmt.Sprintf(`{"id":%d,"created_at":%q,"status":%q,"conclusion":%s,"html_url":"https://example.test/runs/%d"}`, id, r.created, r.status, conclusion, id))
		names := make([]string, 0, len(r.jobs))
		for name := range r.jobs {
			names = append(names, name)
		}
		sort.Strings(names)
		var jobs []string
		for _, name := range names {
			jobs = append(jobs, fmt.Sprintf(`{"name":%q,"conclusion":%q}`, name, r.jobs[name]))
		}
		writeFile(t, filepath.Join(docs, fmt.Sprintf("jobs-%d.json", id)), `{"jobs":[`+strings.Join(jobs, ",")+`]}`)
	}
	writeFile(t, filepath.Join(docs, "self.json"), fmt.Sprintf(`{"id":%d,"workflow_id":77,"created_at":%q}`, labelSelf, labelSelfCreated))
	writeFile(t, filepath.Join(docs, "runs.json"), `{"workflow_runs":[`+strings.Join(listed, ",")+`]}`)
	gh := `#!/usr/bin/env bash
[ "$1" = api ] || { echo "stand-in gh: unexpected $*" >&2; exit 97; }
path="$2"
if [ -n "$BROKEN" ] && [[ "$path" == *"$BROKEN"* ]]; then echo "HTTP 403: Resource not accessible by integration" >&2; exit 1; fi
if [ -n "$GARBLED" ] && [[ "$path" == *"$GARBLED"* ]]; then echo "<html>bad gateway</html>"; exit 0; fi
case "$path" in
  "repos/SocialGouv/iterion/actions/runs/500") cat "$DOCS/self.json" ;;
  "repos/SocialGouv/iterion/actions/workflows/77/runs?head_sha=abc123&event=pull_request&per_page=100") cat "$DOCS/runs.json" ;;
  repos/SocialGouv/iterion/actions/runs/*/jobs\?filter=latest\&per_page=100) id="${path#repos/SocialGouv/iterion/actions/runs/}"; cat "$DOCS/jobs-${id%%/*}.json" ;;
  *) echo "stand-in gh: unexpected path $path" >&2; exit 98 ;;
esac
`
	writeFile(t, filepath.Join(dir, "gh"), gh)
	if err := os.Chmod(filepath.Join(dir, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(jq, filepath.Join(dir, "jq")); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "github_output")
	cmd := exec.Command("bash", "--noprofile", "--norc", script)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(),
		"GITHUB_OUTPUT=" + output, "GITHUB_REPOSITORY=" + prRepo,
		fmt.Sprintf("GITHUB_RUN_ID=%d", labelSelf), "HEAD_SHA=abc123", "LABEL=epic:ci-diet",
		"DOCS=" + docs, "BROKEN=" + broken, "GARBLED=" + garbled,
	}
	b, err := cmd.CombinedOutput()
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v\n%s", labelVerdictScript, err, b)
		}
		code = exit.ExitCode()
	}
	got, _ := os.ReadFile(output)
	return code, string(got), string(b)
}

// TestLabelVerdictRepeatsTheRunBefore RUNS the reader against canned
// histories of the pull request's head commit. The run it repeats is the
// newest earlier run that reached a verdict, by (created_at, id) whatever
// order the API lists runs in — the stand-in lists them by ascending id, so
// each ordering case is served both ways. A cancelled run is passed over; a
// run of an earlier label is repeated like any other. `test` is green only
// when that run's test, vendor-check, golangci and fmt-check each concluded
// success or skipped, `race` only when its race job did; a job missing is
// red. Where it cannot answer — an earlier run still in progress, no earlier
// verdict, an unreadable or garbled history — it exits 1 with an `::error::`
// naming the ways out and writes no output, which the aggregators read as red.
func TestLabelVerdictRepeatsTheRunBefore(t *testing.T) {
	const (
		earlier = "2026-11-26T11:00:00Z"
		older   = "2026-11-26T10:00:00Z"
		later   = "2026-11-26T13:00:00Z"
	)
	green := labelRunWith(earlier, "success", nil)
	red := labelRunWith(earlier, "failure", map[string]string{"test": "failure"})
	for _, tc := range []struct {
		name          string
		runs          map[int]labelRun
		broken, garbl string
		test, race    string // "" when the script must not answer
	}{
		{"a green run before it", map[int]labelRun{400: green}, "", "", "success", "success"},
		{"a red test before it", map[int]labelRun{400: red}, "", "", "failure", "success"},
		{"a red vendor-check alone", map[int]labelRun{400: labelRunWith(earlier, "failure", map[string]string{"vendor-check": "failure"})}, "", "", "failure", "success"},
		{"a red golangci alone", map[int]labelRun{400: labelRunWith(earlier, "failure", map[string]string{"golangci": "failure"})}, "", "", "failure", "success"},
		{"a red fmt-check alone", map[int]labelRun{400: labelRunWith(earlier, "failure", map[string]string{"fmt-check": "cancelled"})}, "", "", "failure", "success"},
		{"a red race alone", map[int]labelRun{400: labelRunWith(earlier, "failure", map[string]string{"race": "failure"})}, "", "", "success", "failure"},
		{"a red brand does not redden test: it runs again in this run", map[int]labelRun{400: labelRunWith(earlier, "failure", map[string]string{"brand": "failure"})}, "", "", "success", "success"},
		{"a skipped required job counts as GitHub counts it", map[int]labelRun{400: labelRunWith(earlier, "success", map[string]string{"vendor-check": "skipped", "fmt-check": "skipped"})}, "", "", "success", "success"},
		{"a required job missing from the run", map[int]labelRun{400: labelRunWith(earlier, "success", map[string]string{"golangci": ""})}, "", "", "failure", "success"},
		{"the race job missing from the run", map[int]labelRun{400: labelRunWith(earlier, "success", map[string]string{"race": ""})}, "", "", "success", "failure"},
		{"a cancelled run is passed over to the green one before it", map[int]labelRun{
			400: labelRunWith(older, "success", nil),
			450: {created: earlier, status: "completed", conclusion: "cancelled", jobs: map[string]string{}},
		}, "", "", "success", "success"},
		{"a cancelled run is passed over to the red one before it", map[int]labelRun{
			400: labelRunWith(older, "failure", map[string]string{"test": "failure"}),
			450: {created: earlier, status: "completed", conclusion: "cancelled", jobs: map[string]string{"test": "skipped"}},
		}, "", "", "failure", "success"},
		{"a red run is not hidden by an older green one (green listed first)", map[int]labelRun{
			400: labelRunWith(older, "success", nil), 450: red,
		}, "", "", "failure", "success"},
		{"a red run is not hidden by an older green one (red listed first)", map[int]labelRun{
			400: red, 450: labelRunWith(older, "success", nil),
		}, "", "", "failure", "success"},
		{"a green run after a red one (red listed first)", map[int]labelRun{
			400: labelRunWith(older, "failure", map[string]string{"test": "failure"}), 450: green,
		}, "", "", "success", "success"},
		{"a green run after a red one (green listed first)", map[int]labelRun{
			400: green, 450: labelRunWith(older, "failure", map[string]string{"test": "failure"}),
		}, "", "", "success", "success"},
		{"a run of an earlier label repeats in turn", map[int]labelRun{
			400: labelRunWith(older, "failure", map[string]string{"test": "failure"}),
			450: labelRunWith(earlier, "success", map[string]string{"vendor-check": "skipped", "golangci": "skipped", "fmt-check": "skipped"}),
		}, "", "", "success", "success"},
		{"a run created after this one is not the run before it", map[int]labelRun{
			400: green, 600: labelRunWith(later, "failure", map[string]string{"test": "failure"}),
		}, "", "", "success", "success"},
		{"a run created in the same second with a lower id is earlier", map[int]labelRun{
			400: labelRunWith(older, "success", nil), 499: labelRunWith(labelSelfCreated, "failure", map[string]string{"test": "failure"}),
		}, "", "", "failure", "success"},
		{"an earlier run still in progress", map[int]labelRun{400: {created: earlier, status: "in_progress", jobs: greenJobs()}}, "", "", "", ""},
		{"no earlier run", nil, "", "", "", ""},
		{"only a cancelled earlier run", map[int]labelRun{400: {created: earlier, status: "completed", conclusion: "cancelled", jobs: map[string]string{}}}, "", "", "", ""},
		{"this run is unreadable", map[int]labelRun{400: green}, "/actions/runs/500", "", "", ""},
		{"the runs of the commit are unreadable", map[int]labelRun{400: green}, "/actions/workflows/", "", "", ""},
		{"the jobs are unreadable", map[int]labelRun{400: green}, "/jobs", "", "", ""},
		{"this run is not JSON", map[int]labelRun{400: green}, "", "/actions/runs/500", "", ""},
		{"the runs of the commit are not JSON", map[int]labelRun{400: green}, "", "/actions/workflows/", "", ""},
		{"the jobs are not JSON", map[int]labelRun{400: green}, "", "/jobs", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, outputs, log := runLabelVerdict(t, tc.runs, tc.broken, tc.garbl)
			if tc.test == "" {
				if code != 1 || outputs != "" {
					t.Errorf("exit %d with outputs %q, want exit 1 and no output — an unanswered verdict is red, never a guess\n%s", code, outputs, log)
				}
				for _, word := range []string{"::error::", "`ci:run`", "`task ci:train`"} {
					if !strings.Contains(log, word) {
						t.Errorf("the refusal does not say %s:\n%s", word, log)
					}
				}
				return
			}
			if want := fmt.Sprintf("test=%s\nrace=%s\n", tc.test, tc.race); code != 0 || outputs != want {
				t.Errorf("exit %d with outputs %q, want exit 0 and %q\n%s", code, outputs, want, log)
			}
		})
	}
}

// TestTheAggregatorsReadThePolicy RUNS each aggregator's step under every
// value prCIEnv can carry, the legs at what such a run gives them: `refused`
// is red with every leg skipped AND with every leg green — the policy, not a
// leg, decides — and says the policy and both ways out; `mirror` is green
// only on a `success` verdict, with every leg skipped; any value GitHub never
// renders is red. event_layers_test.go renders which event gives which value.
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
		run := func(policy, verdict, legs string) (bool, string) {
			vars := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), prCIEnv + "=" + policy, labelVerdictEnv + "=" + verdict, lightRunEnv + "=true"}
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
			green, out := run("refused", "", legs)
			if green {
				t.Errorf("aggregator %q is GREEN on a refused pull request with every leg %s — a required check over nothing", name, legs)
			}
			for _, word := range policyWords {
				if !strings.Contains(out, word) {
					t.Errorf("aggregator %q refuses without saying %s:\n%s", name, word, out)
				}
			}
		}
		if green, out := run("mirror", "success", "skipped"); !green {
			t.Errorf("aggregator %q is RED repeating a green verdict:\n%s", name, out)
		}
		for _, verdict := range []string{"failure", "", "Success", "success "} {
			if green, _ := run("mirror", verdict, "skipped"); green {
				t.Errorf("aggregator %q is GREEN repeating the verdict %q — only `success` repeats green", name, verdict)
			}
		}
		for _, policy := range []string{"", "Run", "RUN", "bogus", "mirror "} {
			if green, _ := run(policy, "success", "success"); green {
				t.Errorf("aggregator %q is GREEN with %s=%q, a value GitHub never renders — red rather than a guess", name, prCIEnv, policy)
			}
		}
		if green, out := run("run", "", "success"); !green {
			t.Errorf("aggregator %q is RED on a run whose legs all succeeded:\n%s", name, out)
		}
	}
	if aggregators != 2 {
		t.Fatalf("found %d aggregators reading %s, want test and race", aggregators, prCIEnv)
	}
}

package ciguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// heavyGateScript is the decider heavyGateJob runs, relative to the repo root
// (the job's step) and to this package (the tests below).
const (
	heavyGateScript     = "scripts/ci/heavy-gate.sh"
	heavyGateScriptPath = "../../" + heavyGateScript
	// heavyWindowEnv is the step's window input: the repository variable,
	// defaulted to six hours. A load-bearing limit keeps its escape hatch
	// (docs/philosophy.md): the variable widens, narrows or, at 0, turns
	// off the debounce.
	heavyWindowEnv   = "HEAVY_EVERY_HOURS"
	heavyWindowValue = "${{ vars.CI_HEAVY_EVERY_HOURS || '6' }}"
)

// TestHeavyLayerRunsOnTheDebouncedPush holds the wiring of the debounced push
// (#3737, docs/adr/173-experimental-delivery-mode.md): heavyGateJob runs on
// push alone, on a GitHub-hosted runner with `actions: read` and a short
// ceiling, exports the script's decision as its `heavy` output, and feeds the
// script the window through CI_HEAVY_EVERY_HOURS with a six-hour default.
// Every job that reads that output `needs:` the gate — a heavy job needs it
// alone — so a push reaches the heavy layer only through the gate's output;
// what each output value does per event is event_layers_test.go's to
// evaluate.
func TestHeavyLayerRunsOnTheDebouncedPush(t *testing.T) {
	var wf struct {
		Jobs map[string]struct {
			If          string            `yaml:"if"`
			RunsOn      any               `yaml:"runs-on"`
			Timeout     int               `yaml:"timeout-minutes"`
			Permissions map[string]string `yaml:"permissions"`
			Outputs     map[string]string `yaml:"outputs"`
			Needs       []string          `yaml:"needs"`
			Env         map[string]string `yaml:"env"`
			Steps       []struct {
				ID  string            `yaml:"id"`
				Run string            `yaml:"run"`
				Env map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	gate, ok := wf.Jobs[heavyGateJob]
	if !ok {
		t.Fatalf("%s has no %q job: nothing decides whether a push runs the heavy layer", workflowPaths[0], heavyGateJob)
	}
	if !sameExpression(gate.If, heavyGateIf) {
		t.Errorf("%q `if:` is %q, want exactly %q: push only, never the merge queue", heavyGateJob, gate.If, heavyGateIf)
	}
	if gate.RunsOn != "ubuntu-latest" {
		t.Errorf("%q runs on %v, want ubuntu-latest: an API read needs no scale-set pod", heavyGateJob, gate.RunsOn)
	}
	if gate.Timeout < 1 || gate.Timeout > 10 {
		t.Errorf("%q timeout-minutes is %d, want 1..10: a decider that hangs holds every heavy leg behind it", heavyGateJob, gate.Timeout)
	}
	if gate.Permissions["actions"] != "read" {
		t.Errorf("%q permissions %v lack `actions: read`: without it the history is unreadable and every push runs heavy", heavyGateJob, gate.Permissions)
	}
	if got, want := gate.Outputs["heavy"], "${{ steps.decide.outputs.heavy }}"; got != want {
		t.Errorf("%q output heavy is %q, want %q", heavyGateJob, got, want)
	}
	decides := false
	for _, st := range gate.Steps {
		if st.ID != "decide" {
			continue
		}
		decides = true
		if strings.TrimSpace(st.Run) != heavyGateScript {
			t.Errorf("%q step decide runs %q, want exactly %q: the script the tests below run is the script CI runs", heavyGateJob, st.Run, heavyGateScript)
		}
		if st.Env[heavyWindowEnv] != heavyWindowValue {
			t.Errorf("%q step decide passes %s=%q, want %q: the window comes from the repository variable, six hours by default", heavyGateJob, heavyWindowEnv, st.Env[heavyWindowEnv], heavyWindowValue)
		}
		if st.Env["GH_TOKEN"] != "${{ github.token }}" {
			t.Errorf("%q step decide passes GH_TOKEN=%q, want the run's own token", heavyGateJob, st.Env["GH_TOKEN"])
		}
	}
	if !decides {
		t.Errorf("%q has no step with id decide", heavyGateJob)
	}

	readers := 0
	for name, job := range wf.Jobs {
		reads := strings.Contains(job.If, "needs."+heavyGateJob+".")
		for _, v := range job.Env {
			reads = reads || strings.Contains(v, "needs."+heavyGateJob+".")
		}
		needsGate := false
		for _, n := range job.Needs {
			needsGate = needsGate || n == heavyGateJob
		}
		heavy := heavyLegs[name] || heavyAdvisoryJobs[name]
		switch {
		case reads && !needsGate:
			t.Errorf("job %q reads needs.%s but does not need it: GitHub renders the output empty there", name, heavyGateJob)
		case heavy && (len(job.Needs) != 1 || !needsGate):
			t.Errorf("heavy job %q needs %v, want exactly [%s]: a heavy job waits on the decider and nothing else", name, job.Needs, heavyGateJob)
		case heavy && !reads:
			t.Errorf("heavy job %q needs %s but its `if:` never reads the decision", name, heavyGateJob)
		case needsGate && !reads:
			t.Errorf("job %q needs %s without reading its output: a need that decides nothing only delays the job", name, heavyGateJob)
		}
		if reads {
			readers++
		}
	}
	if want := len(heavyLegs) + len(heavyAdvisoryJobs) + 2; readers != want {
		t.Errorf("%d jobs read the decision, want %d: the heavy jobs and the two aggregators", readers, want)
	}
}

// heavyHistory is one canned run history of tests.yml on main: run ids with
// their last update, and per run the jobs of its latest attempt.
type heavyHistory struct {
	runs map[int]time.Duration // run id -> age of updated_at
	jobs map[int][]heavyJob
	// broken, when set, is the API path fragment the stand-in gh fails on.
	broken string
	// garbled, when set, is the API path fragment answered with non-JSON.
	garbled string
}

type heavyJob struct {
	name, conclusion string
	age              time.Duration // age of completed_at
}

// runHeavyGate runs the shipped script with a stand-in gh that serves h,
// HEAVY_EVERY_HOURS at window (unset when window is "-"), and returns the
// decision it wrote, its output and the gh calls it made.
func runHeavyGate(t *testing.T, window string, h heavyHistory) (decision, out string, calls int) {
	t.Helper()
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Fatalf("jq is required by %s; it is declared in devbox.json, so run through `devbox run --`: %v", heavyGateScript, err)
	}
	script, err := filepath.Abs(heavyGateScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	dir, docs := t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	stamp := func(age time.Duration) string { return now.Add(-age).Format(time.RFC3339) }
	var runs []string
	for id, age := range h.runs {
		runs = append(runs, fmt.Sprintf(`{"id":%d,"updated_at":%q}`, id, stamp(age)))
		var jobs []string
		for _, j := range h.jobs[id] {
			jobs = append(jobs, fmt.Sprintf(`{"name":%q,"conclusion":%q,"completed_at":%q}`, j.name, j.conclusion, stamp(j.age)))
		}
		writeFile(t, filepath.Join(docs, fmt.Sprintf("jobs-%d.json", id)), `{"jobs":[`+strings.Join(jobs, ",")+`]}`)
	}
	writeFile(t, filepath.Join(docs, "runs.json"), `{"workflow_runs":[`+strings.Join(runs, ",")+`]}`)
	gh := `#!/usr/bin/env bash
echo call >>"$DOCS/calls"
[ "$1" = api ] || { echo "stand-in gh: unexpected $*" >&2; exit 97; }
path="$2"
if [ -n "$BROKEN" ] && [[ "$path" == *"$BROKEN"* ]]; then echo "HTTP 403: Resource not accessible by integration" >&2; exit 1; fi
if [ -n "$GARBLED" ] && [[ "$path" == *"$GARBLED"* ]]; then echo "<html>bad gateway</html>"; exit 0; fi
case "$path" in
  repos/SocialGouv/iterion/actions/workflows/tests.yml/runs\?branch=main\&status=completed*) cat "$DOCS/runs.json" ;;
  repos/SocialGouv/iterion/actions/runs/*/jobs\?filter=latest*) id="${path#repos/SocialGouv/iterion/actions/runs/}"; cat "$DOCS/jobs-${id%%/*}.json" ;;
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
	env := []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(),
		"GITHUB_OUTPUT=" + output, "GITHUB_REPOSITORY=SocialGouv/iterion",
		"DOCS=" + docs, "BROKEN=" + h.broken, "GARBLED=" + h.garbled,
	}
	if window != "-" {
		env = append(env, heavyWindowEnv+"="+window)
	}
	cmd := exec.Command("bash", "--noprofile", "--norc", script)
	cmd.Dir, cmd.Env = t.TempDir(), env
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s exited %v; it exits 0 on every decision:\n%s", heavyGateScript, err, b)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("%s wrote no GITHUB_OUTPUT: %v\n%s", heavyGateScript, err, b)
	}
	logged, _ := os.ReadFile(filepath.Join(docs, "calls"))
	return string(got), string(b), strings.Count(string(logged), "call")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHeavyGateDecidesFromTheRunHistory RUNS the decider against canned
// histories. A push stays light only when a run of the window's latest
// attempts holds a `race-shard` job that concluded success or failure and
// completed inside the window; a skipped or cancelled heavy leg, one that
// completed before the window, or no run at all decides heavy. Every path
// that cannot read the history or the window warns and decides heavy —
// failing toward more testing, never silently toward less — and a window of 0
// decides heavy without reading anything.
func TestHeavyGateDecidesFromTheRunHistory(t *testing.T) {
	ran := func(conclusion string, age time.Duration) heavyHistory {
		return heavyHistory{
			runs: map[int]time.Duration{7: age, 8: 30 * time.Minute},
			jobs: map[int][]heavyJob{
				7: {{"test-unit (0)", "success", age}, {"race-shard (0)", conclusion, age}, {"race-shard (1)", conclusion, age}},
				8: {{"test-unit (0)", "success", 30 * time.Minute}, {"race-shard (0)", "skipped", 30 * time.Minute}},
			},
		}
	}
	for _, tc := range []struct {
		name, window string
		history      heavyHistory
		heavy        bool
		warns        bool
	}{
		{"a heavy run that passed an hour ago", "6", ran("success", time.Hour), false, false},
		{"a heavy run that failed an hour ago still ran", "6", ran("failure", time.Hour), false, false},
		{"heavy legs skipped inside the window", "6", ran("skipped", time.Hour), true, false},
		{"heavy legs cancelled inside the window", "6", ran("cancelled", time.Hour), true, false},
		{"the last heavy run is older than the window", "6", ran("success", 7*time.Hour), true, false},
		{"a wider window reaches it", "24", ran("success", 7*time.Hour), false, false},
		{"the window defaults to nothing in the script", "-", ran("success", time.Hour), true, true},
		{"a run updated inside the window whose heavy leg completed before it", "6", heavyHistory{
			runs: map[int]time.Duration{7: time.Hour},
			jobs: map[int][]heavyJob{7: {{"race-shard (0)", "success", 7 * time.Hour}}},
		}, true, false},
		{"no run on main", "6", heavyHistory{}, true, false},
		{"the run list is unreadable", "6", heavyHistory{broken: "/actions/workflows/"}, true, true},
		{"a run's jobs are unreadable", "6", func() heavyHistory { h := ran("success", time.Hour); h.broken = "/jobs"; return h }(), true, true},
		{"the run list is not JSON", "6", heavyHistory{garbled: "/actions/workflows/"}, true, true},
		{"a run's jobs are not JSON", "6", func() heavyHistory { h := ran("success", time.Hour); h.garbled = "/jobs"; return h }(), true, true},
		{"a window that is not a number", "six", ran("success", time.Hour), true, true},
		{"a negative window", "-1", ran("success", time.Hour), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision, out, _ := runHeavyGate(t, tc.window, tc.history)
			if want := fmt.Sprintf("heavy=%v\n", tc.heavy); decision != want {
				t.Errorf("decision %q, want %q\n%s", decision, want, out)
			}
			if warned := strings.Contains(out, "::warning::"); warned != tc.warns {
				t.Errorf("warned=%v, want %v: an unread history or window is said aloud, a read one is not\n%s", warned, tc.warns, out)
			}
		})
	}
	t.Run("a window of 0 runs heavy on every push without reading the history", func(t *testing.T) {
		decision, out, calls := runHeavyGate(t, "0", heavyHistory{broken: "/"})
		if decision != "heavy=true\n" || calls != 0 || strings.Contains(out, "::warning::") {
			t.Errorf("decision %q after %d gh calls, want heavy=true after none, without a warning\n%s", decision, calls, out)
		}
	})
}

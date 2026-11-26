package ciguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
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

// heavyRun is one canned tests.yml run on main: its trigger, the repository
// its head lives in, its latest attempt's conclusion, the ages of its
// created_at and updated_at, and the jobs of its latest attempt.
type heavyRun struct {
	event, repo, conclusion string
	created, updated        time.Duration
	jobs                    []heavyJob
}

type heavyJob struct {
	name, conclusion string
	age              time.Duration // age of completed_at
}

// heavyHistory is one canned run history, keyed by run id.
type heavyHistory struct {
	runs map[int]heavyRun
	// broken, when set, is the API path fragment the stand-in gh fails on.
	broken string
	// garbled, when set, is the API path fragment answered with non-JSON.
	garbled string
	// attempt, when set, is the GITHUB_RUN_ATTEMPT the script runs under.
	attempt string
}

// heavyRepo is the repository the stand-in gh serves.
const heavyRepo = "SocialGouv/iterion"

// greenRun is a push run of this repository whose latest attempt passed with
// its three race shards green, every timestamp age ago, alongside the `race`
// aggregator and a light leg.
func greenRun(age time.Duration) heavyRun {
	return heavyRun{
		event: "push", repo: heavyRepo, conclusion: "success", created: age, updated: age,
		jobs: []heavyJob{
			{"test-unit (0)", "success", age},
			{"race-shard (0)", "success", age}, {"race-shard (1)", "success", age}, {"race-shard (2)", "success", age},
			{"race", "success", age},
		},
	}
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
	ids := make([]int, 0, len(h.runs))
	for id := range h.runs {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		r := h.runs[id]
		runs = append(runs, fmt.Sprintf(`{"id":%d,"event":%q,"conclusion":%q,"created_at":%q,"updated_at":%q,"head_repository":{"full_name":%q}}`,
			id, r.event, r.conclusion, stamp(r.created), stamp(r.updated), r.repo))
		var jobs []string
		for _, j := range r.jobs {
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
		"GITHUB_OUTPUT=" + output, "GITHUB_REPOSITORY=" + heavyRepo,
		"DOCS=" + docs, "BROKEN=" + h.broken, "GARBLED=" + h.garbled,
	}
	if window != "-" {
		env = append(env, heavyWindowEnv+"="+window)
	}
	if h.attempt != "" {
		env = append(env, "GITHUB_RUN_ATTEMPT="+h.attempt)
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
// histories. A push stays light only behind a GREEN HEAVY RUN inside the
// window: a push, schedule or dispatch run of this repository, created inside
// the window plus two hours, whose latest attempt concluded success with every
// `race-shard (N)` job a success completed inside the window. A red or partial
// heavy run, a fork's `main`, a pull request's run, a rerun of an old run, a
// run whose only race job is the aggregator, or no run at all decides heavy —
// while the heavy layer is red every push stays heavy. Every path that cannot
// read the history or the window warns and decides heavy — failing toward
// more testing, never silently toward less — and a window of 0 decides heavy
// without reading anything.
func TestHeavyGateDecidesFromTheRunHistory(t *testing.T) {
	// with edits a copy: the jobs slice is cloned, so no case mutates another's.
	with := func(r heavyRun, edit func(*heavyRun)) heavyRun { r.jobs = slices.Clone(r.jobs); edit(&r); return r }
	one := func(r heavyRun) heavyHistory { return heavyHistory{runs: map[int]heavyRun{7: r}} }
	shards := func(conclusions ...string) []heavyJob {
		jobs := []heavyJob{{"test-unit (0)", "success", time.Hour}, {"race", "success", time.Hour}}
		for i, c := range conclusions {
			jobs = append(jobs, heavyJob{fmt.Sprintf("race-shard (%d)", i), c, time.Hour})
		}
		return jobs
	}
	green := greenRun(time.Hour)
	for _, tc := range []struct {
		name, window string
		history      heavyHistory
		heavy        bool
		warns        bool
	}{
		{"a green heavy push run an hour ago", "6", one(green), false, false},
		{"a green heavy nightly", "6", one(with(green, func(r *heavyRun) { r.event = "schedule" })), false, false},
		{"a green heavy dispatch", "6", one(with(green, func(r *heavyRun) { r.event = "workflow_dispatch" })), false, false},
		{"a red heavy run keeps the push heavy", "6", one(with(green, func(r *heavyRun) {
			r.conclusion, r.jobs = "failure", shards("success", "failure", "success")
		})), true, false},
		// The shard check alone, isolated from the run's conclusion that
		// would otherwise cover it.
		{"a failed shard in a run reported success", "6", one(with(green, func(r *heavyRun) { r.jobs = shards("success", "failure", "success") })), true, false},
		{"green shards in a run that concluded failure", "6", one(with(green, func(r *heavyRun) { r.conclusion = "failure" })), true, false},
		{"one green shard and two cancelled", "6", one(with(green, func(r *heavyRun) { r.jobs = shards("success", "cancelled", "cancelled") })), true, false},
		{"a timed-out shard", "6", one(with(green, func(r *heavyRun) { r.jobs = shards("success", "timed_out", "success") })), true, false},
		{"skipped shards in a green light run", "6", one(with(green, func(r *heavyRun) { r.jobs = shards("skipped", "skipped", "skipped") })), true, false},
		{"a run whose only race job is the aggregator", "6", one(with(green, func(r *heavyRun) { r.jobs = shards() })), true, false},
		{"a fork's run from its own main", "6", one(with(green, func(r *heavyRun) { r.repo = "someone/iterion" })), true, false},
		{"a pull request run", "6", one(with(green, func(r *heavyRun) { r.event = "pull_request" })), true, false},
		{"a merge-queue run", "6", one(with(green, func(r *heavyRun) { r.event = "merge_group" })), true, false},
		{"a rerun of an old run proves an old sha", "6", one(with(green, func(r *heavyRun) { r.created = 30 * time.Hour })), true, false},
		{"a run created just inside the window plus two hours", "6", one(with(green, func(r *heavyRun) { r.created = 7 * time.Hour })), false, false},
		{"the last green heavy run is older than the window", "6", one(greenRun(7 * time.Hour)), true, false},
		{"a wider window reaches it", "24", one(greenRun(7 * time.Hour)), false, false},
		{"a run updated inside the window whose shards completed before it", "6", one(with(green, func(r *heavyRun) {
			for i := range r.jobs {
				r.jobs[i].age = 7 * time.Hour
			}
		})), true, false},
		// The newest heavy run decides whatever order the API lists runs in:
		// the stand-in lists them by ascending id, so each pair is served in
		// both orders.
		{"a red heavy run is not hidden by an older green one (red listed first)", "6", heavyHistory{runs: map[int]heavyRun{
			8: with(green, func(r *heavyRun) { r.conclusion, r.jobs = "failure", shards("failure", "success", "success") }),
			9: greenRun(3 * time.Hour),
		}}, true, false},
		{"a red heavy run is not hidden by an older green one (green listed first)", "6", heavyHistory{runs: map[int]heavyRun{
			8: greenRun(3 * time.Hour),
			9: with(green, func(r *heavyRun) { r.conclusion, r.jobs = "failure", shards("failure", "success", "success") }),
		}}, true, false},
		{"a green heavy run after a red one (green listed first)", "6", heavyHistory{runs: map[int]heavyRun{
			8: green,
			9: with(greenRun(3*time.Hour), func(r *heavyRun) { r.conclusion = "failure" }),
		}}, false, false},
		{"a green heavy run after a red one (red listed first)", "6", heavyHistory{runs: map[int]heavyRun{
			8: with(greenRun(3*time.Hour), func(r *heavyRun) { r.conclusion = "failure" }),
			9: green,
		}}, false, false},
		{"light runs after a green heavy run pass over to it", "6", heavyHistory{runs: map[int]heavyRun{
			8: with(greenRun(20*time.Minute), func(r *heavyRun) { r.jobs = shards("skipped", "skipped", "skipped") }),
			9: green,
		}}, false, false},
		{"a first attempt behind a green heavy run stays light", "6", func() heavyHistory { h := one(green); h.attempt = "1"; return h }(), false, false},
		{"no run on main", "6", heavyHistory{}, true, false},
		{"the window defaults to nothing in the script", "-", one(green), true, true},
		{"the run list is unreadable", "6", heavyHistory{broken: "/actions/workflows/"}, true, true},
		{"a run's jobs are unreadable", "6", func() heavyHistory { h := one(green); h.broken = "/jobs"; return h }(), true, true},
		{"the run list is not JSON", "6", heavyHistory{garbled: "/actions/workflows/"}, true, true},
		{"a run's jobs are not JSON", "6", func() heavyHistory { h := one(green); h.garbled = "/jobs"; return h }(), true, true},
		{"a window that is not a number", "six", one(green), true, true},
		{"a negative window", "-1", one(green), true, true},
		{"a window past a month", "721", one(green), true, true},
		{"the longest window", "720", one(green), false, false},
		{"a window that overflows bash's integers", "99999999999999999999", one(green), true, true},
		// 2^64 + 6: bash wraps it to a plausible 6.
		{"a window that wraps to a plausible one", "18446744073709551622", one(green), true, true},
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
	// A rerun cannot see itself in a history of completed runs: behind an
	// older green heavy run, a rerun of a red heavy run would re-decide light
	// and erase its own red. It keeps the heavy layer without a read.
	t.Run("a rerun keeps the heavy layer behind a green heavy run, without reading the history", func(t *testing.T) {
		h := heavyHistory{runs: map[int]heavyRun{7: green}, attempt: "2"}
		decision, out, calls := runHeavyGate(t, "6", h)
		if decision != "heavy=true\n" || calls != 0 || strings.Contains(out, "::warning::") || !strings.Contains(out, "attempt 2 reruns this run") {
			t.Errorf("decision %q after %d gh calls, want heavy=true after none, naming the rerun, without a warning\n%s", decision, calls, out)
		}
	})
	t.Run("a window of 0 runs heavy on every push without reading the history", func(t *testing.T) {
		decision, out, calls := runHeavyGate(t, "0", heavyHistory{broken: "/"})
		if decision != "heavy=true\n" || calls != 0 || strings.Contains(out, "::warning::") {
			t.Errorf("decision %q after %d gh calls, want heavy=true after none, without a warning\n%s", decision, calls, out)
		}
	})
}

// TestRaceShardLegsAreNamedAsTheGateReadsThem ties the decider's job-name
// predicate to the workflow: heavy-gate.sh counts exactly the jobs GitHub
// names `race-shard (N)`, which holds while tests.yml's `race-shard` job
// carries no `name:` override and one matrix axis whose values are the shard
// numbers. A renamed leg would never match, and every push would run heavy
// for no reason anyone could read; the script must also spell that exact
// predicate.
func TestRaceShardLegsAreNamedAsTheGateReadsThem(t *testing.T) {
	var wf struct {
		Jobs map[string]struct {
			Name     *string `yaml:"name"`
			Strategy struct {
				Matrix map[string][]any `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	job, ok := wf.Jobs["race-shard"]
	if !ok {
		t.Fatalf("%s has no race-shard job", workflowPaths[0])
	}
	if job.Name != nil {
		t.Errorf("race-shard carries `name: %s`: its legs would no longer be named `race-shard (N)`, the only names %s counts", *job.Name, heavyGateScript)
	}
	if len(job.Strategy.Matrix) != 1 {
		t.Errorf("race-shard's matrix has %d axes, want exactly one: GitHub names a leg after every axis value, `race-shard (0, x)` is not a shard to %s", len(job.Strategy.Matrix), heavyGateScript)
	}
	for axis, values := range job.Strategy.Matrix {
		for _, v := range values {
			if _, ok := v.(int); !ok {
				t.Errorf("race-shard matrix axis %s value %v is not a shard number: its leg would not be named `race-shard (N)`", axis, v)
			}
		}
	}
	const predicate = `test("^race-shard \\([0-9]+\\)$")`
	if !strings.Contains(string(mustRead(t, heavyGateScriptPath)), predicate) {
		t.Errorf("%s no longer selects shards with %s: the legs tests.yml names would drift from what the gate counts", heavyGateScript, predicate)
	}
}

// heavyJudgeCall is how version.yml's release gate asks heavy-gate.sh whether
// the Tests attempt that just completed is a green heavy run (#3741).
const heavyJudgeCall = `bash ` + heavyGateScript + ` --judge-run "$RUN_ID" "$RUN_ATTEMPT"`

// standInJudgeGH serves heavy-gate.sh's judge mode from $DOCS: the attempt
// object and its jobs listing, logging every path asked in $DOCS/paths, and
// failing like an API error on a path containing $BROKEN.
const standInJudgeGH = `#!/usr/bin/env bash
[ "$1" = api ] || { echo "stand-in gh: unexpected $*" >&2; exit 97; }
path="$2"
printf '%s\n' "$path" >>"$DOCS/paths"
if [ -n "${BROKEN:-}" ] && [[ "$path" == *"$BROKEN"* ]]; then echo "HTTP 403: Resource not accessible by integration" >&2; exit 1; fi
case "$path" in
  repos/SocialGouv/iterion/actions/runs/*/attempts/*/jobs\?per_page=100) cat "$DOCS/jobs.json" ;;
  repos/SocialGouv/iterion/actions/runs/*/attempts/*) cat "$DOCS/attempt.json" ;;
  *) echo "stand-in gh: unexpected path $path" >&2; exit 98 ;;
esac
`

// writeJudgeDocs writes run r as attempt 2 of run 7 — its attempt object and
// its jobs listing, either one replaced by non-JSON when garble names it
// ("attempt" or "jobs") — and returns the docs dir and a bin dir holding the
// stand-in gh.
func writeJudgeDocs(t *testing.T, r heavyRun, garble string) (docs, bin string) {
	t.Helper()
	docs, bin = t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	attempt := fmt.Sprintf(`{"id":7,"run_attempt":2,"event":%q,"conclusion":%q,"head_repository":{"full_name":%q}}`, r.event, r.conclusion, r.repo)
	var jobs []string
	for _, j := range r.jobs {
		jobs = append(jobs, fmt.Sprintf(`{"name":%q,"conclusion":%q,"completed_at":%q}`, j.name, j.conclusion, now.Add(-j.age).Format(time.RFC3339)))
	}
	listing := `{"total_count":` + fmt.Sprint(len(jobs)) + `,"jobs":[` + strings.Join(jobs, ",") + `]}`
	switch garble {
	case "attempt":
		attempt = "<html>bad gateway</html>"
	case "jobs":
		listing = "<html>bad gateway</html>"
	}
	writeFile(t, filepath.Join(docs, "attempt.json"), attempt)
	writeFile(t, filepath.Join(docs, "jobs.json"), listing)
	writeFile(t, filepath.Join(bin, "gh"), standInJudgeGH)
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	return docs, bin
}

// judgePaths returns the API paths the stand-in gh was asked for.
func judgePaths(t *testing.T, docs string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(docs, "paths"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// TestHeavyGateJudgesOneRun RUNS heavy-gate.sh's judge mode — version.yml's
// release gate — over canned attempts. It answers with the SAME predicate as
// the window decision above (one jq definition in the script): this
// repository's push, schedule or dispatch run, the attempt concluded success,
// every `race-shard (N)` job a success. Minus the window: a release judges the
// run that just completed. Exactly one line on stdout, green or not-green,
// exit 0; exit 2 and nothing on stdout when the attempt cannot be read.
func TestHeavyGateJudgesOneRun(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatalf("jq is required by %s; it is declared in devbox.json, so run through `devbox run --`: %v", heavyGateScript, err)
	}
	script, err := filepath.Abs(heavyGateScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	with := func(r heavyRun, edit func(*heavyRun)) heavyRun { r.jobs = slices.Clone(r.jobs); edit(&r); return r }
	shards := func(conclusions ...string) []heavyJob {
		jobs := []heavyJob{{"test-unit (0)", "success", time.Hour}, {"race", "success", time.Hour}}
		for i, c := range conclusions {
			jobs = append(jobs, heavyJob{fmt.Sprintf("race-shard (%d)", i), c, time.Hour})
		}
		return jobs
	}
	// A release judges a run whatever its age: shards that completed long
	// ago still prove it.
	green := greenRun(30 * time.Hour)
	for _, tc := range []struct {
		name           string
		run            heavyRun
		garble, broken string
		args           []string
		want           string // stdout line; "" with code 2
		code           int
		reason         string
	}{
		{name: "a green heavy run", run: green, want: "green", reason: "every race-shard (N) job a success"},
		{name: "a green heavy nightly", run: with(green, func(r *heavyRun) { r.event = "schedule" }), want: "green"},
		{name: "a red shard", run: with(green, func(r *heavyRun) { r.jobs = shards("success", "failure", "success") }), want: "not-green", reason: "race shards red"},
		{name: "a cancelled shard", run: with(green, func(r *heavyRun) { r.jobs = shards("success", "cancelled", "success") }), want: "not-green", reason: "race shards red"},
		{name: "green shards in a run that concluded failure", run: with(green, func(r *heavyRun) { r.conclusion = "failure" }), want: "not-green", reason: "concluded failure"},
		{name: "a run whose only race job is the aggregator", run: with(green, func(r *heavyRun) { r.jobs = shards() }), want: "not-green", reason: "race shards light"},
		{name: "a light run, every shard skipped", run: with(green, func(r *heavyRun) { r.jobs = shards("skipped", "skipped", "skipped") }), want: "not-green", reason: "race shards light"},
		{name: "a fork's run", run: with(green, func(r *heavyRun) { r.repo = "someone/iterion" }), want: "not-green", reason: "not a push, schedule or dispatch run"},
		{name: "a pull request run", run: with(green, func(r *heavyRun) { r.event = "pull_request" }), want: "not-green", reason: "not a push, schedule or dispatch run"},
		{name: "an attempt that is not JSON", run: green, garble: "attempt", code: 2, reason: "cannot read"},
		{name: "jobs that are not JSON", run: green, garble: "jobs", code: 2, reason: "cannot read the jobs"},
		{name: "an API error", run: green, broken: "/attempts/", code: 2, reason: "cannot read"},
		{name: "a run id that is not a number", run: green, args: []string{"--judge-run", "7;x", "2"}, code: 2, reason: "is not a number"},
		{name: "a missing attempt", run: green, args: []string{"--judge-run", "7"}, code: 2, reason: "usage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs, bin := writeJudgeDocs(t, tc.run, tc.garble)
			args := tc.args
			if args == nil {
				args = []string{"--judge-run", "7", "2"}
			}
			cmd := exec.Command("bash", append([]string{"--noprofile", "--norc", script}, args...)...)
			cmd.Dir = t.TempDir()
			cmd.Env = []string{
				"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
				"HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(),
				"GITHUB_REPOSITORY=" + heavyRepo, "DOCS=" + docs, "BROKEN=" + tc.broken,
			}
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			code := 0
			if err := cmd.Run(); err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("run %s: %v", heavyGateScript, err)
				}
				code = exit.ExitCode()
			}
			if code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, stdout.String(), stderr.String())
			}
			want := ""
			if tc.want != "" {
				want = tc.want + "\n"
			}
			if stdout.String() != want {
				t.Errorf("stdout %q, want %q — exactly the verdict line, the reason goes to stderr\nstderr: %s", stdout.String(), want, stderr.String())
			}
			if tc.reason != "" && !strings.Contains(stderr.String(), tc.reason) {
				t.Errorf("stderr does not name the reason %q:\n%s", tc.reason, stderr.String())
			}
			if tc.args == nil && tc.code == 0 && !strings.Contains(judgePaths(t, docs), "repos/SocialGouv/iterion/actions/runs/7/attempts/2/jobs") {
				t.Errorf("the judge did not read the jobs of run 7 attempt 2: %q", judgePaths(t, docs))
			}
		})
	}
}

// TestTheHeavyPredicateHasOneDefinition holds the point of the judge mode:
// the window decision and the release gate read ONE jq definition of a green
// heavy run, so neither can drift from the other. Each definition appears once
// in heavy-gate.sh, and each mode's program calls it.
func TestTheHeavyPredicateHasOneDefinition(t *testing.T) {
	src := string(mustRead(t, heavyGateScriptPath))
	for _, def := range []string{"def heavy_candidate(", "def shard_verdict(", `test("^race-shard`, "green_heavy() {"} {
		if n := strings.Count(src, def); n != 1 {
			t.Errorf("%s spells %q %d times, want exactly once: a second definition of a green heavy run drifts from the first", heavyGateScript, def, n)
		}
	}
	for _, call := range []string{"select(heavy_candidate($repo))", "shard_verdict($cutoff)", `shard_verdict("")`, "if heavy_candidate($repo)"} {
		if !strings.Contains(src, call) {
			t.Errorf("%s no longer calls %q: one mode stopped reading the shared predicate", heavyGateScript, call)
		}
	}
	if n := strings.Count(src, `green_heavy "$conclusion" "$verdict"`); n != 2 {
		t.Errorf("%s calls green_heavy %d times, want 2 — once per mode", heavyGateScript, n)
	}
}

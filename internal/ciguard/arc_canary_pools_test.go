package ciguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The ARC canary probes every runner pool (#3701): one probe job per pool,
// routed like the jobs it stands for, a freshness check once the pool is set,
// and a watchdog that judges each pool on its own job and folds the answers.

const (
	canaryAggregateScript = "../../scripts/ci/arc-canary-aggregate.sh"
	runnerFreshnessScript = "../../scripts/ci/arc-runner-freshness.sh"
)

// canaryPools are the pools of #3694: probe job, routing variable.
var canaryPools = []struct{ job, variable string }{
	{"canary-small", "CI_SMALL_RUNNER"},
	{"canary-large", "CI_LARGE_RUNNER"},
	{"canary-heavy", "CI_HEAVY_RUNNER"},
}

// runScriptWith runs a shipped script with extra environment and stdin.
func runScriptWith(t *testing.T, script string, env map[string]string, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{scriptUnderTest(t, script)}, args...)...)
	cmd.Env = scriptEnv(t)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v\nstderr: %s", script, err, errb.String())
		}
		code = exit.ExitCode()
	}
	return out.String(), errb.String(), code
}

func keyOf(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimPrefix(line, key+"=")
		}
	}
	return "<absent>"
}

// TestTheCanaryProbesEveryPoolAsItsJobsAreRouted reads the shipped workflow:
// one probe per pool, routed through the pool's variable with the operator's
// lever in front, a freshness step that runs only once the pool is set (the
// legacy set predates the baked image and would page forever), and a watchdog
// that names every probe it judges.
//
// Mutations seen red: the heavy probe deleted; canary-large routed through
// CI_SMALL_RUNNER; the small freshness step's `if:` dropped; canary-heavy
// removed from the watchdog's pool list.
func TestTheCanaryProbesEveryPoolAsItsJobsAreRouted(t *testing.T) {
	var wf struct {
		Jobs map[string]struct {
			RunsOn string `yaml:"runs-on"`
			Steps  []struct {
				If   string `yaml:"if"`
				Uses string `yaml:"uses"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(mustRead(t, canaryWorkflowPath), &wf); err != nil {
		t.Fatalf("parse %s: %v", canaryWorkflowPath, err)
	}
	if _, legacy := wf.Jobs["canary"]; legacy {
		t.Errorf("%s still carries the single `canary` probe — a pool without its own probe goes deaf unseen", canaryWorkflowPath)
	}
	var watchdog string
	for _, st := range wf.Jobs[canaryWatchdogJob].Steps {
		watchdog += st.Run
	}
	probes := 0
	for name := range wf.Jobs {
		if strings.HasPrefix(name, "canary-") {
			probes++
			if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` [0-9]+$`).MatchString(watchdog) {
				t.Errorf("probe %q is not in the watchdog's pool list — it would run every period and never be judged", name)
			}
		}
	}
	if probes != len(canaryPools) {
		t.Errorf("%s carries %d canary-* probes, want %d (one per pool)", canaryWorkflowPath, probes, len(canaryPools))
	}
	for _, p := range canaryPools {
		job, ok := wf.Jobs[p.job]
		if !ok {
			t.Errorf("%s has no probe %q", canaryWorkflowPath, p.job)
			continue
		}
		want := "${{ vars.CI_SELF_HOSTED == 'off' && 'ubuntu-latest' || vars." + p.variable + " || 'arc-runners' }}"
		if job.RunsOn != want {
			t.Errorf("probe %q runs on %q, want %q — a probe routed apart from its pool measures another machine", p.job, job.RunsOn, want)
		}
		gate := "vars." + p.variable + " != '' && vars.CI_SELF_HOSTED != 'off'"
		fresh := false
		for _, st := range job.Steps {
			if strings.Contains(st.Run, "scripts/ci/arc-runner-freshness.sh") || strings.HasPrefix(st.Uses, "actions/checkout@") {
				if st.If != gate {
					t.Errorf("probe %q step %q is gated by %q, want %q", p.job, st.Run+st.Uses, st.If, gate)
				}
			}
			if strings.Contains(st.Run, "scripts/ci/arc-runner-freshness.sh") {
				fresh = true
			}
		}
		if !fresh {
			t.Errorf("probe %q does not check its pool's image against the tree", p.job)
		}
	}
	for _, needle := range []string{"./scripts/ci/arc-canary-aggregate.sh", "CANARY_STALL_SECONDS=", "JOB_STATUS="} {
		if !strings.Contains(watchdog, needle) {
			t.Errorf("the watchdog does not pass %q — each pool must be judged on its own job and threshold, then folded", needle)
		}
	}
}

// TestTheCanaryVerdictJudgesEachPoolOnItsOwnJob: one pool still waiting keeps
// the whole run in progress, so the run's age must not redden a pool whose own
// probe finished, and a pool's threshold is its own.
//
// Mutations seen red: the JOB_STATUS branch removed (the finished small pool
// turns red on the run's age); CANARY_STALL_SECONDS ignored (heavy red at
// 40 minutes); the override's number check dropped.
func TestTheCanaryVerdictJudgesEachPoolOnItsOwnJob(t *testing.T) {
	base := map[string]string{"PREV_STATUS": "in_progress", "PREV_CREATED": canaryProbeCreated, "NOW": "2026-10-29T03:40:00Z"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name  string
		facts map[string]string
		want  string
		code  int
	}{
		{"finished pool in a run still in progress", with(map[string]string{"JOB_STATUS": "completed", "JOB_CONCLUSION": "success", "JOB_CREATED": canaryProbeCreated, "JOB_STARTED": canaryProbeStarted}), "green", 0},
		{"pool queued 40 min, default threshold", with(map[string]string{"JOB_STATUS": "queued", "JOB_CREATED": canaryProbeCreated}), "red", 0},
		{"pool queued 40 min, heavy threshold", with(map[string]string{"JOB_STATUS": "queued", "JOB_CREATED": canaryProbeCreated, "CANARY_STALL_SECONDS": "5400"}), "green", 0},
		{"pool queued, no creation date", with(map[string]string{"JOB_STATUS": "queued"}), "", 2},
		{"threshold that is not a number", with(map[string]string{"JOB_STATUS": "queued", "JOB_CREATED": canaryProbeCreated, "CANARY_STALL_SECONDS": "90m"}), "", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			verdict, _, log, code := canaryVerdict(t, c.facts)
			if code != c.code || verdict != c.want {
				t.Errorf("verdict=%q exit=%d, want %q exit %d\nlog: %s", verdict, code, c.want, c.code, log)
			}
		})
	}
}

// TestTheCanaryAggregatePagesOnAnyRedPool exercises the fold.
//
// Mutations seen red: green checked before red (a red pool hidden by a green
// one); an unknown verdict counted as skip; an empty input answered skip.
func TestTheCanaryAggregatePagesOnAnyRedPool(t *testing.T) {
	cases := []struct {
		name, in, verdict, detail string
		code                      int
	}{
		{"one red among greens", "small\tgreen\t\nlarge\tred\tqueued 1800s\nheavy\tgreen\t\n", "red", "large: queued 1800s", 0},
		{"two reds", "small\tred\ta\nlarge\tgreen\t\nheavy\tred\tb\n", "red", "small: a; heavy: b", 0},
		{"greens and skips", "small\tgreen\t\nlarge\tskip\tno job\nheavy\tskip\tno job\n", "green", "", 0},
		{"only skips", "small\tskip\tx\nlarge\tskip\tx\nheavy\tskip\tx\n", "skip", "no pool had a probe to judge", 0},
		{"an answer that is no verdict", "small\tgreen\t\nlarge\tmaybe\t\n", "<absent>", "<absent>", 2},
		{"no answer at all", "", "<absent>", "<absent>", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, stderr, code := runScriptWith(t, canaryAggregateScript, nil, c.in)
			if code != c.code || keyOf(out, "verdict") != c.verdict || keyOf(out, "detail") != c.detail {
				t.Errorf("got verdict=%q detail=%q exit=%d, want %q %q exit %d\nstderr: %s",
					keyOf(out, "verdict"), keyOf(out, "detail"), code, c.verdict, c.detail, c.code, stderr)
			}
		})
	}
}

// TestTheRunnerFreshnessNamesEveryDriftedFact drives the freshness check over
// a crafted tree and crafted image facts, then over THIS tree with the facts
// its own Dockerfile would bake — the parser must read the real file.
//
// Mutations seen red: the baked-actions comparison removed; the Node check
// removed; a missing image env file read as fresh; the version-line filter
// removed (the real runner's log line read as its version).
func TestTheRunnerFreshnessNamesEveryDriftedFact(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"ci/arc-runner/Dockerfile":        "# syntax\nFROM ghcr.io/actions/actions-runner:2.337.0@sha256:abc\nARG GO_VERSION=1.26.0\nARG NODE_VERSION=24.22.1\n",
		"ci/arc-runner/baked-actions.txt": "actions/checkout@aaa\nactions/setup-go@bbb\n",
	})
	img := t.TempDir()
	writeFiles(t, img, map[string]string{
		"env":     "GO_VERSION=1.26.0\nNODE_VERSION=24.22.1\n",
		"actions": "actions/checkout@aaa\nactions/setup-go@bbb\n",
	})
	env := func(runner, envFile, actionsFile string) map[string]string {
		return map[string]string{"IMAGE_RUNNER_VERSION": runner, "IMAGE_ENV_FILE": envFile, "IMAGE_ACTIONS_FILE": actionsFile}
	}
	if out, _, code := runScriptWith(t, runnerFreshnessScript, env("2.337.0", filepath.Join(img, "env"), filepath.Join(img, "actions")), "", root); code != 0 {
		t.Fatalf("a matching image is reported stale (exit %d):\n%s", code, out)
	}
	// What Runner.Listener really prints inside a job (seen on the pools
	// 2026-11-25): a log line first, then the version.
	noisy := "[RUNNER 2026-11-25 18:34:33Z INFO HostContext] No proxy settings were found based on environmental variables (http_proxy/https_proxy/HTTP_PROXY/HTTPS_PROXY)\n2.337.0"
	if out, _, code := runScriptWith(t, runnerFreshnessScript, env(noisy, filepath.Join(img, "env"), filepath.Join(img, "actions")), "", root); code != 0 {
		t.Errorf("the runner's real --version answer (a log line, then the version) is read as stale (exit %d):\n%s", code, out)
	}
	if out, _, code := runScriptWith(t, runnerFreshnessScript, env("[RUNNER] no version here", filepath.Join(img, "env"), filepath.Join(img, "actions")), "", root); code != 2 {
		t.Errorf("an answer with no version line exited %d, want 2 (unreadable is not fresh):\n%s", code, out)
	}
	stale := t.TempDir()
	writeFiles(t, stale, map[string]string{
		"node":    "GO_VERSION=1.26.0\nNODE_VERSION=22.0.0\n",
		"actions": "actions/checkout@aaa\n",
	})
	drift := []struct{ name, runner, envFile, actionsFile, fact string }{
		{"runner", "2.330.0", filepath.Join(img, "env"), filepath.Join(img, "actions"), "stale runner-version"},
		{"node", "2.337.0", filepath.Join(stale, "node"), filepath.Join(img, "actions"), "stale node-version"},
		{"actions", "2.337.0", filepath.Join(img, "env"), filepath.Join(stale, "actions"), "stale baked-actions"},
	}
	for _, d := range drift {
		t.Run(d.name, func(t *testing.T) {
			out, _, code := runScriptWith(t, runnerFreshnessScript, env(d.runner, d.envFile, d.actionsFile), "", root)
			if code != 1 || !strings.Contains(out, d.fact) {
				t.Errorf("exit=%d, want 1 naming %q:\n%s", code, d.fact, out)
			}
		})
	}
	if out, _, code := runScriptWith(t, runnerFreshnessScript, env("2.337.0", filepath.Join(img, "absent"), filepath.Join(img, "actions")), "", root); code != 2 {
		t.Errorf("an image without its env file answered exit %d, want 2 (unreadable is not fresh):\n%s", code, out)
	}

	// This tree, with the facts its own Dockerfile bakes.
	df := string(mustRead(t, runnerDockerfilePath))
	arg := func(name string) string {
		m := regexp.MustCompile(`(?m)^ARG ` + name + `=(.+)$`).FindStringSubmatch(df)
		if m == nil {
			t.Fatalf("%s declares no ARG %s", runnerDockerfilePath, name)
		}
		return m[1]
	}
	from := regexp.MustCompile(`(?m)^FROM ghcr\.io/actions/actions-runner:([0-9.]+)@`).FindStringSubmatch(df)
	if from == nil {
		t.Fatalf("%s has no pinned FROM ghcr.io/actions/actions-runner:<version>@<digest>", runnerDockerfilePath)
	}
	here := t.TempDir()
	writeFiles(t, here, map[string]string{"env": "GO_VERSION=" + arg("GO_VERSION") + "\nNODE_VERSION=" + arg("NODE_VERSION") + "\n"})
	if err := os.WriteFile(filepath.Join(here, "actions"), mustRead(t, bakedManifestPath), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _, code := runScriptWith(t, runnerFreshnessScript, env(from[1], filepath.Join(here, "env"), filepath.Join(here, "actions")), "", "../.."); code != 0 {
		t.Errorf("the image this tree builds is reported stale against this tree (exit %d):\n%s", code, out)
	}
}

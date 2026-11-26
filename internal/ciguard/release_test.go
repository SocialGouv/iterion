package ciguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// releaseWorkflowPath is the workflow that cuts every release: its commit and
// tag land on main by a direct push.
const releaseWorkflowPath = "../../.github/workflows/version.yml"

type releaseStep struct {
	ID   string            `yaml:"id"`
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	If   string            `yaml:"if"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

type releaseWorkflow struct {
	On struct {
		WorkflowDispatch *yaml.Node `yaml:"workflow_dispatch"`
		WorkflowRun      *struct {
			Workflows []string `yaml:"workflows"`
			Types     []string `yaml:"types"`
			Branches  []string `yaml:"branches"`
		} `yaml:"workflow_run"`
		PullRequest       *yaml.Node `yaml:"pull_request"`
		PullRequestTarget *yaml.Node `yaml:"pull_request_target"`
		Push              *yaml.Node `yaml:"push"`
		Schedule          *yaml.Node `yaml:"schedule"`
		MergeGroup        *yaml.Node `yaml:"merge_group"`
	} `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress *bool  `yaml:"cancel-in-progress"`
		Queue            string `yaml:"queue"`
	} `yaml:"concurrency"`
	Jobs map[string]struct {
		If    string            `yaml:"if"`
		Env   map[string]string `yaml:"env"`
		Steps []releaseStep     `yaml:"steps"`
	} `yaml:"jobs"`
}

func readReleaseWorkflow(t *testing.T) releaseWorkflow {
	t.Helper()
	src, err := os.ReadFile(releaseWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", releaseWorkflowPath, err)
	}
	var wf releaseWorkflow
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatalf("parse %s: %v", releaseWorkflowPath, err)
	}
	return wf
}

// TestReleaseRunsNeverOverlap holds the one property whose loss costs a tag.
// When its push is rejected, release-it rolls back by deleting the remote tag
// of the version it computed; if another run had just pushed that same
// version, the tag it deletes is the other run's — a release commit left on
// main with no tag. So every trigger shares one group, nothing cancels a run
// in progress, and `queue: max` keeps every pending run: by default a group
// keeps ONE pending run and silently replaces it with the next, which would
// let the run following an ordinary Tests completion replace a pending
// heavy-run release or dispatch.
func TestReleaseRunsNeverOverlap(t *testing.T) {
	c := readReleaseWorkflow(t).Concurrency
	if c.Group != "${{ github.workflow }}" {
		t.Errorf("version.yml's concurrency group is %q, want %q — one group for every trigger: a key built from the event, the ref or the pull request lets two release-its run at once. Another constant would do as well; if that is the change, update this test with it", c.Group, "${{ github.workflow }}")
	}
	if c.CancelInProgress == nil || *c.CancelInProgress {
		t.Errorf("version.yml must say `cancel-in-progress: false` explicitly — a release cancelled mid-run can stop between its push and the GitHub release it creates")
	}
	if c.Queue != "max" {
		t.Errorf("version.yml's concurrency has `queue: %q`, want \"max\" — without it a pending run is silently replaced by the next one", c.Queue)
	}
}

// The automatic release rule (#3741): a release follows a green HEAVY run of
// 🧪 Tests on main, at most once a day, for the commit that run proved.
const (
	releaseJobIf = "github.event_name == 'workflow_dispatch' || (github.event_name == 'workflow_run' && github.event.workflow_run.conclusion == 'success' && github.event.workflow_run.head_branch == 'main' && github.event.workflow_run.head_repository.full_name == github.repository && contains(fromJSON('[\"push\", \"schedule\", \"workflow_dispatch\"]'), github.event.workflow_run.event))"

	// releaseMinTagAgeHours is the daily cap the operator set: 20 h, not 24,
	// so a heavy run that drifts a few hours earlier the next day still
	// releases.
	releaseMinTagAgeHours = "20"

	heavyStepID   = "heavy"
	dailyStepID   = "daily"
	tipStepID     = "tip"
	changesStepID = "changes"
	majorStepID   = "major"
	releaseItName = "Run release-it"

	// raceShardJob is the tests.yml job whose legs make a run heavy.
	raceShardJob = "race-shard"
)

// releaseSteps returns version.yml's `version` job and the index of each step
// the release rule is made of, failing when one is gone.
func releaseSteps(t *testing.T) (steps []releaseStep, at map[string]int) {
	t.Helper()
	job, ok := readReleaseWorkflow(t).Jobs["version"]
	if !ok {
		t.Fatalf("%s has no `version` job", releaseWorkflowPath)
	}
	at = map[string]int{}
	for i, st := range job.Steps {
		key := st.ID
		if st.Name == releaseItName {
			key = releaseItName
		}
		if key != "" {
			at[key] = i
		}
	}
	for _, want := range []string{heavyStepID, dailyStepID, tipStepID, changesStepID, majorStepID, releaseItName} {
		if _, ok := at[want]; !ok {
			t.Fatalf("the version job lost the step %q this guard reads — renamed? Keep the ids in step with this test", want)
		}
	}
	return job.Steps, at
}

// TestReleaseFollowsAGreenHeavyRunOfMain pins the automatic release rule's
// shape: what triggers it, what the job admits, and the chain of steps each
// gating the next. Expressions are compared byte for byte, like the routing
// expressions in routing_test.go: one token flipped releases after a light
// run, after a pull request's run, or more than once a day — each a
// green-looking run.
func TestReleaseFollowsAGreenHeavyRunOfMain(t *testing.T) {
	wf := readReleaseWorkflow(t)

	// Triggers: a dispatch, and the completion of Tests on main — nothing that
	// releases after each merge.
	if wf.On.WorkflowDispatch == nil {
		t.Errorf("version.yml lost its workflow_dispatch trigger — the operator's way to release a fix that cannot wait")
	}
	for name, node := range map[string]*yaml.Node{
		"pull_request": wf.On.PullRequest, "pull_request_target": wf.On.PullRequestTarget,
		"push": wf.On.Push, "schedule": wf.On.Schedule, "merge_group": wf.On.MergeGroup,
	} {
		if node != nil {
			t.Errorf("version.yml triggers on %s — a release follows a green heavy run of main (workflow_run) or a dispatch, nothing else (#3741)", name)
		}
	}
	var tests struct {
		Name string `yaml:"name"`
		Jobs map[string]struct {
			Name     string    `yaml:"name"`
			Strategy yaml.Node `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(mustRead(t, testsWorkflowPath), &tests); err != nil {
		t.Fatalf("parse %s: %v", testsWorkflowPath, err)
	}
	run := wf.On.WorkflowRun
	if run == nil {
		t.Errorf("version.yml has no workflow_run trigger — no green heavy run of main would ever release")
	} else {
		if len(run.Workflows) != 1 || run.Workflows[0] != tests.Name {
			t.Errorf("version.yml follows workflow_run %q, want exactly [%q] — tests.yml's name, byte for byte: a workflow_run naming a workflow that does not exist never fires, and says nothing", run.Workflows, tests.Name)
		}
		if len(run.Types) != 1 || run.Types[0] != "completed" {
			t.Errorf("version.yml's workflow_run types are %q, want [completed]", run.Types)
		}
		if len(run.Branches) != 1 || run.Branches[0] != "main" {
			t.Errorf("version.yml's workflow_run branches are %q, want [main] — a pull request's or a merge group's Tests run proves no commit of main", run.Branches)
		}
	}

	// The heavy-run proof reads job names: the race-shard job must exist under
	// that id, with no display name of its own, its legs named `race-shard (N)`.
	shard, ok := tests.Jobs[raceShardJob]
	switch {
	case !ok:
		t.Errorf("%s has no %q job — the heavy-run proof in version.yml reads its legs by name and would fail every run", testsWorkflowPath, raceShardJob)
	case shard.Name != "":
		t.Errorf("%s's %q job carries `name: %s` — its legs would no longer be called %q and the heavy-run proof would find none", testsWorkflowPath, raceShardJob, shard.Name, raceShardJob)
	}

	job := wf.Jobs["version"]
	if !sameExpression(job.If, releaseJobIf) {
		t.Errorf("version.yml's version job runs on `if: %s`, want `if: %s` — a dispatch, or a Tests run of this repository's main that succeeded and came from a push, the schedule or a dispatch", job.If, releaseJobIf)
	}
	if got := job.Env["MIN_TAG_AGE_HOURS"]; got != releaseMinTagAgeHours {
		t.Errorf("version.yml's daily cap MIN_TAG_AGE_HOURS is %q, want %q — the operator's decision of 2026-11-26 (#3741)", got, releaseMinTagAgeHours)
	}
	if got := wf.Permissions["actions"]; got != "read" {
		t.Errorf("version.yml grants `actions: %s`, want `actions: read` — the heavy-run proof lists the Tests run's jobs", got)
	}
	if got, ok := wf.Permissions["pull-requests"]; ok {
		t.Errorf("version.yml grants `pull-requests: %s` — nothing in the release reads a pull request any more", got)
	}

	steps, at := releaseSteps(t)
	order := []string{heavyStepID, dailyStepID, tipStepID, changesStepID, majorStepID, releaseItName}
	for i := 1; i < len(order); i++ {
		if at[order[i-1]] >= at[order[i]] {
			t.Errorf("the version job runs %q at step %d and %q at step %d — the release rule is a chain: heavy proof, daily cap, move to the proven tip, nothing-new check, major hold, release-it", order[i-1], at[order[i-1]], order[i], at[order[i]])
		}
	}
	for _, c := range []struct{ step, want, why string }{
		{heavyStepID, "github.event_name == 'workflow_run'",
			"every automatic run proves its Tests run heavy; a dispatch does not"},
		{dailyStepID, "github.event_name == 'workflow_dispatch' || steps.heavy.outputs.proven == 'true'",
			"the cap answers a dispatch (which it lets through) and a proven heavy run, nothing else"},
		{tipStepID, "steps.daily.outputs.due == 'true'",
			"main's tip is checked against the proven commit only once the cap let the run through"},
		{changesStepID, "steps.tip.outputs.release == 'true'",
			"the nothing-new check runs only on the proven tip"},
		{majorStepID, "steps.changes.outputs.has_changes == 'true'",
			"the major hold judges exactly the runs release-it would run"},
		{releaseItName, "steps.changes.outputs.has_changes == 'true' && steps.major.outputs.verdict == 'release'",
			"no run cuts unless every gate before it answered yes and the major hold answered release"},
	} {
		if got := steps[at[c.step]].If; got != c.want {
			t.Errorf("step %q runs under `if: %s`, want `if: %s` — %s. If the change is deliberate, update this test with it", c.step, got, c.want, c.why)
		}
	}
	// Every step that installs, configures or pushes waits on the gates.
	for i, st := range steps {
		if i <= at[dailyStepID] || i == at[tipStepID] || i >= at[changesStepID] {
			continue
		}
		if st.If != "steps.daily.outputs.due == 'true'" {
			t.Errorf("step %d (%s%s) runs under `if: %s`, want `steps.daily.outputs.due == 'true'` — a run the heavy proof or the daily cap turned away installs nothing", i, st.Name, st.Uses, st.If)
		}
	}
	for _, c := range []struct{ step, env, want string }{
		{heavyStepID, "RUN_ID", "${{ github.event.workflow_run.id }}"},
		{heavyStepID, "RUN_ATTEMPT", "${{ github.event.workflow_run.run_attempt }}"},
		{heavyStepID, "PROVEN_SHA", "${{ github.event.workflow_run.head_sha }}"},
		{dailyStepID, "EVENT", "${{ github.event_name }}"},
		{tipStepID, "EVENT", "${{ github.event_name }}"},
		{tipStepID, "PROVEN_SHA", "${{ github.event.workflow_run.head_sha }}"},
	} {
		if got := steps[at[c.step]].Env[c.env]; got != c.want {
			t.Errorf("step %q receives %s as %q, want %q — the fact must come from the event through env", c.step, c.env, got, c.want)
		}
	}
	// "A green heavy run" has ONE definition, heavy-gate.sh's, which also
	// debounces the heavy layer (#3737): the proof calls its judge mode on the
	// attempt that completed and holds no predicate of its own.
	if heavyRun := steps[at[heavyStepID]].Run; !strings.Contains(heavyRun, heavyJudgeCall) {
		t.Errorf("the heavy-run proof does not call `%s` — a second definition of a green heavy run would drift from the debounce's", heavyJudgeCall)
	} else if strings.Contains(heavyRun, "race-shard") || strings.Contains(heavyRun, "gh api") {
		t.Errorf("the heavy-run proof reads jobs or names race-shard itself — the predicate lives in %s alone", heavyGateScript)
	}
	for _, id := range []string{heavyStepID, dailyStepID, tipStepID} {
		if strings.Contains(steps[at[id]].Run, "${{") {
			t.Errorf("step %q's run embeds a GitHub expression — pass event values through env", id)
		}
	}
}

// stepRun is one execution of a version.yml step's own `run:` text.
type stepRun struct {
	code   int
	log    string
	output map[string]string
}

// runReleaseStep runs the `run:` text of the version job's step `id` the way
// GitHub runs a bash step (`bash --noprofile --norc -eo pipefail`), in dir,
// with the job's env, the given step env and an isolated git, and returns
// its exit code, log and $GITHUB_OUTPUT.
func runReleaseStep(t *testing.T, id, dir string, env map[string]string, path ...string) stepRun {
	t.Helper()
	wf := readReleaseWorkflow(t)
	steps, at := releaseSteps(t)
	script := filepath.Join(t.TempDir(), id+".sh")
	if err := os.WriteFile(script, []byte(steps[at[id]].Run), 0o600); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(t.TempDir(), "github_output")
	cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", script)
	cmd.Dir = dir
	cmd.Env = append(scriptEnv(t),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GITHUB_OUTPUT="+outFile,
		"GITHUB_REPOSITORY=SocialGouv/iterion",
	)
	for k, v := range wf.Jobs["version"].Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if len(path) > 0 {
		for i, kv := range cmd.Env {
			if strings.HasPrefix(kv, "PATH=") {
				cmd.Env[i] = "PATH=" + strings.Join(path, string(os.PathListSeparator)) + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
			}
		}
	}
	out, err := cmd.CombinedOutput()
	r := stepRun{log: string(out), output: map[string]string{}}
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run step %q: %v\n%s", id, err, out)
		}
		r.code = exit.ExitCode()
	}
	written, err := os.ReadFile(outFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(written), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			r.output[k] = v
		}
	}
	return r
}

// TestTheHeavyProofAsksTheHeavyGate runs the heavy-run proof's own text, from
// the repository root as CI does, against the stand-in gh heavy_debounce_test.go
// serves heavy-gate.sh's judge mode with: a green heavy run proves the sha, any
// other run releases nothing with a notice, and an attempt the gate cannot read
// fails the step. What counts as green is heavy-gate.sh's, tested there.
func TestTheHeavyProofAsksTheHeavyGate(t *testing.T) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	green := greenRun(time.Hour)
	red := greenRun(time.Hour)
	red.conclusion = "failure"
	for _, tc := range []struct {
		name       string
		run        heavyRun
		garble     string
		wantCode   int
		wantProven string
		wantLog    string
	}{
		{name: "a green heavy run proves the sha", run: green, wantProven: "true", wantLog: "proved 0123456789abcdef"},
		{name: "a red heavy run releases nothing", run: red, wantProven: "false", wantLog: "is not a green heavy run"},
		{name: "an unreadable attempt fails the step", run: green, garble: "jobs", wantCode: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs, bin := writeJudgeDocs(t, tc.run, tc.garble)
			r := runReleaseStep(t, heavyStepID, root, map[string]string{
				"DOCS": docs, "RUN_ID": "7", "RUN_ATTEMPT": "2",
				"RUN_URL":    "https://github.com/SocialGouv/iterion/actions/runs/7",
				"PROVEN_SHA": "0123456789abcdef0123456789abcdef01234567",
			}, bin)
			if r.code != tc.wantCode {
				t.Fatalf("exit %d, want %d\nlog: %s", r.code, tc.wantCode, r.log)
			}
			if got := r.output["proven"]; got != tc.wantProven {
				t.Errorf("proven=%q, want %q\nlog: %s", got, tc.wantProven, r.log)
			}
			if tc.wantLog != "" && !strings.Contains(r.log, tc.wantLog) {
				t.Errorf("log does not say %q:\n%s", tc.wantLog, r.log)
			}
			if asked := judgePaths(t, docs); !strings.Contains(asked, "/actions/runs/7/attempts/2/jobs") {
				t.Errorf("the proof asked gh for %q — the jobs of the attempt that completed (7, attempt 2), not the run's latest", asked)
			}
		})
	}
}

// tagAged creates an annotated tag at HEAD whose creation date is age ago.
func tagAged(t *testing.T, dir, tag string, age time.Duration) {
	t.Helper()
	cmd := gittest.Cmd(dir, "tag", "-a", "-m", tag, tag)
	cmd.Env = append(cmd.Env, "GIT_COMMITTER_DATE="+time.Now().Add(-age).UTC().Format(time.RFC3339))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git tag %s: %v\n%s", tag, err, out)
	}
}

// TestTheDailyCapLetsOneAutomaticReleaseADay runs the cap's own text over
// repositories whose newest v* tag has a given age.
func TestTheDailyCapLetsOneAutomaticReleaseADay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		event   string
		tags    map[string]time.Duration
		wantDue string
		wantLog string
	}{
		{name: "a tag cut two hours ago holds the release", event: "workflow_run",
			tags: map[string]time.Duration{"v7.40.0": 50 * time.Hour, "v7.41.0": 2 * time.Hour}, wantDue: "false", wantLog: "under the 20 h daily cap"},
		{name: "a tag cut nineteen hours ago still holds it", event: "workflow_run",
			tags: map[string]time.Duration{"v7.41.0": 19 * time.Hour}, wantDue: "false", wantLog: "v7.41.0 was cut 19 h ago"},
		{name: "a tag cut twenty-one hours ago lets it through", event: "workflow_run",
			tags: map[string]time.Duration{"v7.41.0": 21 * time.Hour}, wantDue: "true"},
		// The 20 h boundary, a minute on each side.
		{name: "a tag cut 19h59 ago still holds it", event: "workflow_run",
			tags: map[string]time.Duration{"v7.41.0": 19*time.Hour + 59*time.Minute}, wantDue: "false", wantLog: "under the 20 h daily cap"},
		{name: "a tag cut 20h01 ago lets it through", event: "workflow_run",
			tags: map[string]time.Duration{"v7.41.0": 20*time.Hour + time.Minute}, wantDue: "true", wantLog: "v7.41.0 was cut 20 h ago"},
		// The newest tag is the newest by CREATION, not by version or name: a
		// maintenance tag cut an hour ago below an older, higher release
		// still counts as today's release.
		{name: "a recent tag with a lower version holds the release", event: "workflow_run",
			tags: map[string]time.Duration{"v7.41.0": 30 * time.Hour, "v7.40.1": time.Hour}, wantDue: "false", wantLog: "v7.40.1 was cut 1 h ago"},
		{name: "a recent tag whose name sorts below an older one holds the release", event: "workflow_run",
			tags: map[string]time.Duration{"v9.9.0": 30 * time.Hour, "v10.0.1": time.Hour}, wantDue: "false", wantLog: "v10.0.1 was cut 1 h ago"},
		{name: "a young tag outside v* does not count", event: "workflow_run",
			tags: map[string]time.Duration{"v7.41.0": 30 * time.Hour, "desktop-nightly": time.Hour}, wantDue: "true"},
		{name: "a dispatch ignores the cap", event: "workflow_dispatch",
			tags: map[string]time.Duration{"v7.41.0": time.Hour}, wantDue: "true", wantLog: "a dispatch ignores the daily cap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := gittest.SourceRepo(t)
			for tag, age := range tc.tags {
				tagAged(t, dir, tag, age)
			}
			r := runReleaseStep(t, dailyStepID, dir, map[string]string{"EVENT": tc.event})
			if r.code != 0 {
				t.Fatalf("exit %d\nlog: %s", r.code, r.log)
			}
			if got := r.output["due"]; got != tc.wantDue {
				t.Errorf("due=%q, want %q\nlog: %s", got, tc.wantDue, r.log)
			}
			if tc.wantLog != "" && !strings.Contains(r.log, tc.wantLog) {
				t.Errorf("log does not say %q:\n%s", tc.wantLog, r.log)
			}
		})
	}
}

// TestTheReleaseCutsOnlyTheProvenCommit runs the move to main's tip over a
// checkout whose origin moved since: an automatic run releases only when the
// tip is the commit the heavy run proved, and names both commits otherwise.
func TestTheReleaseCutsOnlyTheProvenCommit(t *testing.T) {
	origin := gittest.SourceRepo(t)
	checkedOut := gittest.Run(t, origin, "rev-parse", "HEAD")
	work := filepath.Join(t.TempDir(), "work")
	gittest.Run(t, filepath.Dir(work), "clone", "-q", origin, work)
	tip := commit(t, origin, "feat: merged after the checkout", map[string]string{"main.go": "package main\n"})

	for _, tc := range []struct {
		name, event, proven, wantRelease string
		wantLog                          []string
	}{
		{name: "the proven commit is main's tip", event: "workflow_run", proven: tip, wantRelease: "true"},
		{name: "main moved past the proven commit", event: "workflow_run", proven: checkedOut, wantRelease: "false",
			wantLog: []string{"No release: main is at " + tip, "not " + checkedOut}},
		{name: "a dispatch releases the tip as it finds it", event: "workflow_dispatch", proven: "", wantRelease: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gittest.Run(t, work, "checkout", "-q", "-B", "main", checkedOut)
			r := runReleaseStep(t, tipStepID, work, map[string]string{"EVENT": tc.event, "PROVEN_SHA": tc.proven})
			if r.code != 0 {
				t.Fatalf("exit %d\nlog: %s", r.code, r.log)
			}
			if got := r.output["release"]; got != tc.wantRelease {
				t.Errorf("release=%q, want %q\nlog: %s", got, tc.wantRelease, r.log)
			}
			for _, want := range tc.wantLog {
				if !strings.Contains(r.log, want) {
					t.Errorf("log does not say %q:\n%s", want, r.log)
				}
			}
		})
	}
}

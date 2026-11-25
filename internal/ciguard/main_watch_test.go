package ciguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// mainWatchPath is the workflow that reruns, quarantines and reverts on main
// (#3692). It runs on workflow_run with this repository's secrets, an App
// token and write scopes, whatever triggered the run it judges — so every
// line of its trust boundary is pinned here, one guard per line.
const mainWatchPath = "../../.github/workflows/main-watch.yml"

// mainWatchCondition is the job's `if:`, byte for byte: this repository
// only, and for a workflow_run event only a push or schedule run of main from
// THIS repository. `branches: [main]` alone also matches a fork's pull
// request from a branch named main.
const mainWatchCondition = "github.repository == 'SocialGouv/iterion' && ( github.event_name != 'workflow_run' || ( github.event.workflow_run.head_branch == 'main' && github.event.workflow_run.head_repository.full_name == github.repository && (github.event.workflow_run.event == 'push' || github.event.workflow_run.event == 'schedule')))"

type mainWatchWorkflow struct {
	On          map[string]yaml.Node `yaml:"on"`
	Permissions yaml.Node            `yaml:"permissions"`
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress *bool  `yaml:"cancel-in-progress"`
		Queue            string `yaml:"queue"`
	} `yaml:"concurrency"`
	Jobs map[string]struct {
		If          string            `yaml:"if"`
		Permissions map[string]string `yaml:"permissions"`
		Steps       []struct {
			Name string            `yaml:"name"`
			Uses string            `yaml:"uses"`
			With map[string]string `yaml:"with"`
			Env  map[string]string `yaml:"env"`
			Run  string            `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readMainWatch(t *testing.T) mainWatchWorkflow {
	t.Helper()
	var wf mainWatchWorkflow
	if err := yaml.Unmarshal(mustRead(t, mainWatchPath), &wf); err != nil {
		t.Fatalf("parse %s: %v", mainWatchPath, err)
	}
	if len(wf.Jobs) != 1 || len(wf.Jobs["watch"].Steps) == 0 {
		t.Fatalf("%s: want the one job `watch`, got %d job(s)", mainWatchPath, len(wf.Jobs))
	}
	return wf
}

// The triggers are exactly the three the boundary was written for. A
// pull_request_target (or any pull-request trigger) would hand this job's
// tokens to an event a stranger controls; the watched workflow is named
// exactly as tests.yml names itself, so a rename there cannot leave main-watch
// listening to nothing.
func TestMainWatchTriggersAreTheWatchedRunTheScheduleAndADispatch(t *testing.T) {
	wf := readMainWatch(t)
	var triggers []string
	for k := range wf.On {
		triggers = append(triggers, k)
	}
	slices.Sort(triggers)
	if want := []string{"schedule", "workflow_dispatch", "workflow_run"}; !slices.Equal(triggers, want) {
		t.Fatalf("main-watch triggers = %v, want %v", triggers, want)
	}
	var run struct {
		Workflows []string `yaml:"workflows"`
		Types     []string `yaml:"types"`
		Branches  []string `yaml:"branches"`
	}
	node := wf.On["workflow_run"]
	if err := node.Decode(&run); err != nil {
		t.Fatal(err)
	}
	var tests struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &tests); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(run.Workflows, []string{tests.Name}) || !slices.Equal(run.Types, []string{"completed"}) || !slices.Equal(run.Branches, []string{"main"}) {
		t.Fatalf("workflow_run = %+v, want workflows [%q], types [completed], branches [main]", run, tests.Name)
	}
	var dispatch struct {
		Inputs map[string]struct {
			Default any `yaml:"default"`
		} `yaml:"inputs"`
	}
	node = wf.On["workflow_dispatch"]
	if err := node.Decode(&dispatch); err != nil {
		t.Fatal(err)
	}
	if d := dispatch.Inputs["dry_run"].Default; d != true {
		t.Errorf("a dispatch must default to the dry run (dry_run default %v): a hand-started run acts only when asked to", d)
	}
}

// Least privilege: nothing at the workflow level, and the job holds exactly
// what its writes need.
func TestMainWatchHoldsOnlyTheScopesItsWritesNeed(t *testing.T) {
	wf := readMainWatch(t)
	if wf.Permissions.Kind != yaml.MappingNode || len(wf.Permissions.Content) != 0 {
		t.Errorf("main-watch's workflow-level permissions must be `{}`, got %q", wf.Permissions.Value)
	}
	want := map[string]string{"contents": "read", "actions": "write", "issues": "write", "pull-requests": "write", "id-token": "write"}
	got := wf.Jobs["watch"].Permissions
	if len(got) != len(want) {
		t.Fatalf("job permissions = %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("job permission %s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

func TestMainWatchRunsOnlyForMainRunsOfThisRepository(t *testing.T) {
	wf := readMainWatch(t)
	if got := strings.Join(strings.Fields(wf.Jobs["watch"].If), " "); got != mainWatchCondition {
		t.Fatalf("main-watch's job condition is\n  %s\nwant\n  %s", got, mainWatchCondition)
	}
}

// Every checkout is of main, and nothing the watched run carries reaches a
// `with:`: checking out its head would run a branch's code with the App
// token in .git/config.
func TestMainWatchChecksOutMainNeverTheWatchedHead(t *testing.T) {
	wf := readMainWatch(t)
	checkouts := 0
	for _, s := range wf.Jobs["watch"].Steps {
		for k, v := range s.With {
			if strings.Contains(v, "github.event") || strings.Contains(v, "inputs.") {
				t.Errorf("step %q: with.%s = %q reads the event or an input", s.Name, k, v)
			}
		}
		if strings.HasPrefix(s.Uses, "actions/checkout@") {
			checkouts++
			if s.With["ref"] != "main" {
				t.Errorf("step %q checks out %q, want main", s.Name, s.With["ref"])
			}
		}
	}
	if checkouts == 0 {
		t.Fatal("main-watch checks nothing out: the harvest no longer matches the workflow")
	}
}

// No expression is interpolated into a shell: an input or an event field
// reaches `run:` through `env:` only, so a dispatch value cannot become code.
func TestMainWatchInterpolatesNothingIntoAShell(t *testing.T) {
	wf := readMainWatch(t)
	for _, s := range wf.Jobs["watch"].Steps {
		if strings.Contains(s.Run, "${{") {
			t.Errorf("step %q interpolates an expression into its shell:\n%s", s.Name, s.Run)
		}
	}
}

// One judge at a time and no event dropped: GitHub keeps one pending run per
// group by default and silently replaces it with the next.
func TestMainWatchJudgesOneRunAtATimeAndDropsNone(t *testing.T) {
	c := readMainWatch(t).Concurrency
	if c.Group != "${{ github.workflow }}" || c.CancelInProgress == nil || *c.CancelInProgress || c.Queue != "max" {
		t.Fatalf("concurrency = group %q, cancel-in-progress %v, queue %q; want ${{ github.workflow }}, false, max", c.Group, c.CancelInProgress, c.Queue)
	}
}

// The Judge step is executed with a fake Go per event: what is observed is
// what the shell hands to scripts/ci/mainwatch.
func TestMainWatchJudgesWhatEachEventNames(t *testing.T) {
	wf := readMainWatch(t)
	var run string
	for _, s := range wf.Jobs["watch"].Steps {
		if strings.Contains(s.Run, "./scripts/ci/mainwatch") {
			run = s.Run
		}
	}
	if run == "" {
		t.Fatal("no step runs ./scripts/ci/mainwatch")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	const fakeGo = "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GO_ARGS\"\n"
	for _, tc := range []struct {
		name string
		env  []string
		want string // the arguments after ./scripts/ci/mainwatch; "" = the step must fail
	}{
		{"workflow_run", []string{"EVENT=workflow_run", "WATCHED_RUN_ID=42"}, "-run-id 42"},
		{"schedule", []string{"EVENT=schedule"}, "-sweep"},
		{"dispatch, dry run of one run", []string{"EVENT=workflow_dispatch", "INPUT_RUN_ID=42", "INPUT_DRY_RUN=true"}, "-run-id 42 -dry-run"},
		{"dispatch, live sweep", []string{"EVENT=workflow_dispatch", "INPUT_RUN_ID=", "INPUT_DRY_RUN=false"}, "-sweep"},
		{"another event", []string{"EVENT=pull_request_target"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "go"), []byte(fakeGo), 0o755); err != nil {
				t.Fatal(err)
			}
			argsFile := filepath.Join(dir, "args")
			cmd := exec.Command(bash, "--noprofile", "--norc", "-c", run)
			cmd.Dir = dir
			cmd.Env = append([]string{"PATH=" + dir + string(os.PathListSeparator) + "/usr/bin:/bin", "HOME=" + dir, "GO_ARGS=" + argsFile}, tc.env...)
			out, runErr := cmd.CombinedOutput()
			if tc.want == "" {
				if runErr == nil {
					t.Fatalf("the step accepted an event it does not handle: %s", out)
				}
				if _, err := os.Stat(argsFile); err == nil {
					t.Fatal("mainwatch ran for an event it does not handle")
				}
				return
			}
			if runErr != nil {
				t.Fatalf("step: %v\n%s", runErr, out)
			}
			raw, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("mainwatch never ran: %v", err)
			}
			args := strings.Fields(string(raw))
			at := slices.Index(args, "./scripts/ci/mainwatch")
			if at < 0 {
				t.Fatalf("go %v does not run ./scripts/ci/mainwatch", args)
			}
			if got := strings.Join(args[at+1:], " "); got != tc.want {
				t.Fatalf("mainwatch %s, want mainwatch %s", got, tc.want)
			}
		})
	}
}

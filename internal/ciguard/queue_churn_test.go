package ciguard

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// churnWorkflowPath carries the janitor that cancels the merge-queue runs an
// entry's new generation replaced.
const churnWorkflowPath = "../../.github/workflows/merge-queue-churn.yml"

// churnJob is the one job in that file.
const churnJob = "cancel-superseded"

// churnWorkflow reads the file the janitor runs from. The guards below judge
// the shipped text — the filter, the triggers, the token — rather than a copy
// of it kept here, because a copy is what drifts.
type churnWorkflow struct {
	On struct {
		MergeGroup any `yaml:"merge_group"`
	} `yaml:"on"`
	Jobs map[string]struct {
		RunsOn      string            `yaml:"runs-on"`
		Permissions map[string]string `yaml:"permissions"`
		Env         map[string]string `yaml:"env"`
		Steps       []struct {
			Name string `yaml:"name"`
			Uses string `yaml:"uses"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readChurnWorkflow(t *testing.T) churnWorkflow {
	t.Helper()
	var wf churnWorkflow
	if err := yaml.Unmarshal(mustRead(t, churnWorkflowPath), &wf); err != nil {
		t.Fatalf("parse %s: %v", churnWorkflowPath, err)
	}
	if _, ok := wf.Jobs[churnJob]; !ok {
		t.Fatalf("%s defines no job %q — the file no longer has the shape these guards read", churnWorkflowPath, churnJob)
	}
	return wf
}

// supersededFilter returns the jq program the shipped workflow hands to jq.
func supersededFilter(t *testing.T) string {
	t.Helper()
	filter := strings.TrimSpace(readChurnWorkflow(t).Jobs[churnJob].Env["SUPERSEDED_FILTER"])
	if filter == "" {
		t.Fatalf("job %q in %s carries no SUPERSEDED_FILTER — the predicate these guards exercise is gone", churnJob, churnWorkflowPath)
	}
	return filter
}

// run is one entry of the GitHub Actions runs listing, in the shape the
// filter reads.
type run struct {
	ID         int64  `json:"id"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	HeadBranch string `json:"head_branch"`
	CreatedAt  string `json:"created_at"`
}

const (
	selfBranch  = "gh-readonly-queue/main/pr-2570-aaaaaaaaaaaa"
	selfID      = "4000"
	selfCreated = "2026-10-28T12:00:00Z"
)

// selfRun is this generation's own run, always present in the listing.
var selfRun = run{ID: 4000, Event: "merge_group", Status: "in_progress", HeadBranch: selfBranch, CreatedAt: selfCreated}

// cancelTargets runs the SHIPPED filter over a crafted listing and returns
// the run ids it selects for cancellation.
func cancelTargets(t *testing.T, filter string, runs []run) []string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"workflow_runs": runs})
	if err != nil {
		t.Fatalf("marshal the run listing: %v", err)
	}
	cmd := exec.Command("jq", "-r",
		"--arg", "self_branch", selfBranch,
		"--arg", "self_id", selfID,
		"--arg", "self_created", selfCreated,
		filter)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("jq refused the shipped filter (%v)\nstderr: %s\nfilter: %s", err, stderr.String(), filter)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ids = append(ids, line)
		}
	}
	return ids
}

// TestTheSupersededFilterCancelsTheReplacedGenerationAndNothingElse holds the
// janitor's whole contract to the text that ships, one case per clause.
//
// The expensive side is a wrongly cancelled run: a cancelled required check
// reads as a red entry and ejects the pull request, so every case that must
// NOT cancel is a case this guard names. The cheap side — a superseded run
// left running — is the first case.
//
// The witness: mute any clause of the filter in the workflow (treat a
// superseded run as current — drop the `created_at` comparison, or compare
// the full branch instead of the entry key) and the matching case here goes
// red on its own assertion.
func TestTheSupersededFilterCancelsTheReplacedGenerationAndNothingElse(t *testing.T) {
	filter := supersededFilter(t)
	for _, tc := range []struct {
		name string
		runs []run
		want []string
	}{
		{
			name: "an earlier generation of this entry is cancelled",
			runs: []run{selfRun, {ID: 3000, Event: "merge_group", Status: "in_progress", HeadBranch: "gh-readonly-queue/main/pr-2570-bbbbbbbbbbbb", CreatedAt: "2026-10-28T11:00:00Z"}},
			want: []string{"3000"},
		},
		{
			name: "every generation this one replaced, however many",
			runs: []run{
				selfRun,
				{ID: 3000, Event: "merge_group", Status: "in_progress", HeadBranch: "gh-readonly-queue/main/pr-2570-bbbbbbbbbbbb", CreatedAt: "2026-10-28T11:00:00Z"},
				{ID: 2000, Event: "merge_group", Status: "queued", HeadBranch: "gh-readonly-queue/main/pr-2570-cccccccccccc", CreatedAt: "2026-10-28T10:00:00Z"},
			},
			want: []string{"2000", "3000"},
		},
		{
			name: "a sibling job of this very generation shares the branch and survives",
			runs: []run{selfRun, {ID: 4001, Event: "merge_group", Status: "in_progress", HeadBranch: selfBranch, CreatedAt: "2026-10-28T11:59:00Z"}},
			want: nil,
		},
		{
			name: "this run never cancels itself",
			runs: []run{selfRun},
			want: nil,
		},
		{
			name: "another entry of the same group is untouched",
			runs: []run{selfRun, {ID: 3001, Event: "merge_group", Status: "in_progress", HeadBranch: "gh-readonly-queue/main/pr-2571-bbbbbbbbbbbb", CreatedAt: "2026-10-28T11:00:00Z"}},
			want: nil,
		},
		{
			name: "an entry whose number merely starts with ours is untouched",
			runs: []run{selfRun, {ID: 3002, Event: "merge_group", Status: "in_progress", HeadBranch: "gh-readonly-queue/main/pr-25700-bbbbbbbbbbbb", CreatedAt: "2026-10-28T11:00:00Z"}},
			want: nil,
		},
		{
			name: "an entry queued on another base branch is untouched",
			runs: []run{selfRun, {ID: 3003, Event: "merge_group", Status: "in_progress", HeadBranch: "gh-readonly-queue/release/pr-2570-bbbbbbbbbbbb", CreatedAt: "2026-10-28T11:00:00Z"}},
			want: nil,
		},
		{
			name: "a finished run is not cancelled",
			runs: []run{selfRun, {ID: 3004, Event: "merge_group", Status: "completed", HeadBranch: "gh-readonly-queue/main/pr-2570-bbbbbbbbbbbb", CreatedAt: "2026-10-28T11:00:00Z"}},
			want: nil,
		},
		{
			name: "a LATER generation is not cancelled: the newer one does the cancelling",
			runs: []run{selfRun, {ID: 5000, Event: "merge_group", Status: "in_progress", HeadBranch: "gh-readonly-queue/main/pr-2570-bbbbbbbbbbbb", CreatedAt: "2026-10-28T13:00:00Z"}},
			want: nil,
		},
		{
			name: "an identical timestamp is not an order, so nothing is cancelled",
			runs: []run{selfRun, {ID: 3005, Event: "merge_group", Status: "in_progress", HeadBranch: "gh-readonly-queue/main/pr-2570-bbbbbbbbbbbb", CreatedAt: selfCreated}},
			want: nil,
		},
		{
			name: "a pull-request or push run is never a queue generation",
			runs: []run{
				selfRun,
				{ID: 3006, Event: "push", Status: "in_progress", HeadBranch: "gh-readonly-queue/main/pr-2570-bbbbbbbbbbbb", CreatedAt: "2026-10-28T11:00:00Z"},
				{ID: 3007, Event: "pull_request", Status: "in_progress", HeadBranch: "feat/whatever", CreatedAt: "2026-10-28T11:00:00Z"},
			},
			want: nil,
		},
		{
			name: "a branch the key cannot be read out of is skipped, not guessed",
			runs: []run{
				selfRun,
				{ID: 3008, Event: "merge_group", Status: "in_progress", HeadBranch: "gh-readonly-queue/main/pr-2570", CreatedAt: "2026-10-28T11:00:00Z"},
				{ID: 3009, Event: "merge_group", Status: "in_progress", HeadBranch: "main", CreatedAt: "2026-10-28T11:00:00Z"},
			},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The listing's order is GitHub's, and nothing in it is a
			// promise: the same inputs in the reverse order must give the
			// same verdict, or this filter reads an accident.
			forward := cancelTargets(t, filter, tc.runs)
			reversed := make([]run, len(tc.runs))
			for i, r := range tc.runs {
				reversed[len(tc.runs)-1-i] = r
			}
			backward := cancelTargets(t, filter, reversed)
			for _, got := range []struct {
				order string
				ids   []string
			}{{"as listed", forward}, {"reversed", backward}} {
				if strings.Join(got.ids, ",") != strings.Join(tc.want, ",") {
					t.Errorf("%s: the filter cancels %v, want %v", got.order, got.ids, tc.want)
				}
			}
		})
	}
}

// TestTheChurnJanitorRunsOnlyInsideTheQueueWithTheOneScopeItNeeds holds the
// three properties that bound what the janitor can do, each of which was a
// deliberate choice:
//
//   - it triggers on `merge_group` ALONE. On `pull_request` or `push` the
//     same filter would see runs whose branch it cannot key, and a trigger
//     is not the place to discover that.
//   - its token carries `actions: write` and nothing else. The scope is the
//     only one that can cancel a run; the job-level block replaces the
//     workflow default, so a widened set here is a widened set on a job that
//     runs the queue entry's own file.
//   - it uses no action and checks out nothing. The step body is the whole
//     program — visible in the diff of the pull request that changes it —
//     and nothing is fetched from codeload on a runner that gets 429s
//     (#2435).
func TestTheChurnJanitorRunsOnlyInsideTheQueueWithTheOneScopeItNeeds(t *testing.T) {
	wf := readChurnWorkflow(t)
	src := string(mustRead(t, churnWorkflowPath))

	var triggers struct {
		On map[string]any `yaml:"on"`
	}
	if err := yaml.Unmarshal([]byte(src), &triggers); err != nil {
		t.Fatalf("parse the triggers of %s: %v", churnWorkflowPath, err)
	}
	if len(triggers.On) != 1 {
		t.Errorf("%s triggers on %v — the janitor runs inside the merge queue alone, where every branch carries an entry key", churnWorkflowPath, keysOf(triggers.On))
	} else if _, ok := triggers.On["merge_group"]; !ok {
		t.Errorf("%s triggers on %v, want merge_group alone", churnWorkflowPath, keysOf(triggers.On))
	}

	job := wf.Jobs[churnJob]
	if len(job.Permissions) != 1 || job.Permissions["actions"] != "write" {
		t.Errorf("job %q carries permissions %v, want exactly {actions: write} — the one scope that cancels a run, on a job that runs the queue entry's own workflow file", churnJob, job.Permissions)
	}
	for _, st := range job.Steps {
		if st.Uses != "" {
			t.Errorf("job %q step %q uses %q — the janitor fetches no action: it must still start when codeload answers 429 (#2435)", churnJob, st.Name, st.Uses)
		}
	}
	if len(job.Steps) == 0 {
		t.Errorf("job %q has no step — nothing cancels anything", churnJob)
	}
}

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

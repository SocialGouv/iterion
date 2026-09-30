package ciguard

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// releaseWorkflowPath is the workflow that cuts every release: its commit and
// tag land on main by a direct push, which invalidates every merge group the
// queue is building.
const releaseWorkflowPath = "../../.github/workflows/version.yml"

type releaseWorkflow struct {
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress *bool  `yaml:"cancel-in-progress"`
		Queue            string `yaml:"queue"`
	} `yaml:"concurrency"`
	Jobs map[string]struct {
		Steps []struct {
			ID   string `yaml:"id"`
			Name string `yaml:"name"`
			If   string `yaml:"if"`
		} `yaml:"steps"`
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
// let a merged pull request's run (held while the queue builds) replace a
// pending nightly or dispatch.
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

// TestReleaseReadsTheQueueBeforeMovingToMainsTip holds the three steps that
// decide what a merged pull request releases: their order, and the condition
// each runs under. The queue is read FIRST: a group in flight at that read is
// the only queue merge that could land before release-it pushes, and it holds
// the release; whatever lands before the move to main's tip is in the tree
// release-it commits onto. Read after that move, a queue merge could land
// between the two — the queue then reads empty, release-it commits onto a
// tree older than main and its push is rejected, a red run on main.
//
// The conditions are compared byte for byte, like the routing expressions in
// routing_test.go: one token flipped in any of them skips every merged pull
// request's release in silence, releases while a group builds, or skips the
// move to main's tip — each a green-looking run.
func TestReleaseReadsTheQueueBeforeMovingToMainsTip(t *testing.T) {
	job, ok := readReleaseWorkflow(t).Jobs["version"]
	if !ok {
		t.Fatalf("%s has no `version` job", releaseWorkflowPath)
	}
	const (
		holdID     = "queue"
		tipName    = "Release main's tip as it is now"
		releaseRun = "Run release-it"
	)
	hold, tip, release := -1, -1, -1
	var holdIf, tipIf, releaseIf string
	for i, st := range job.Steps {
		switch {
		case st.ID == holdID:
			hold, holdIf = i, st.If
		case st.Name == tipName:
			tip, tipIf = i, st.If
		case st.Name == releaseRun:
			release, releaseIf = i, st.If
		}
	}
	if hold < 0 || tip < 0 || release < 0 {
		t.Fatalf("the version job lost a step this guard reads (step id %q at %d, %q at %d, %q at %d) — renamed? Keep the names in step with this test", holdID, hold, tipName, tip, releaseRun, release)
	}
	if hold >= tip || tip >= release {
		t.Errorf("the version job runs the queue hold at step %d, the move to main's tip at step %d and release-it at step %d — the hold must come first and release-it last", hold, tip, release)
	}
	for _, c := range []struct{ step, got, want, why string }{
		{"the queue hold", holdIf, "github.event_name == 'pull_request'",
			"only a merged pull request's release waits for the queue; the nightly and a dispatch never read it"},
		{"the move to main's tip", tipIf, "steps.queue.outputs.busy != 'true'",
			"every run that may release moves to main's tip first — the nightly and a dispatch, whose hold is skipped, included"},
		{"release-it", releaseIf, "steps.changes.outputs.has_changes != 'false' && (github.event_name != 'pull_request' || steps.queue.outputs.busy == 'false')",
			"a merged pull request releases only once the hold answered busy=false, the nightly and a dispatch only when something changed since the last tag"},
	} {
		if c.got != c.want {
			t.Errorf("%s runs under `if: %s`, want `if: %s` — %s. If the change is deliberate, update this test with it", c.step, c.got, c.want, c.why)
		}
	}
}

package dispatcher

import (
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// newMinimalDispatcher returns a barely-initialised Dispatcher suitable
// for in-package unit tests that exercise pure helpers like
// [Dispatcher.buildSpec]. It deliberately omits the actor loop —
// callers MUST NOT call Start/Stop on the result.
func newMinimalDispatcher(t *testing.T) *Dispatcher {
	t.Helper()
	d := &Dispatcher{
		logger: iterlog.New(iterlog.LevelError, nopWriter{}),
		state:  newState(),
	}
	return d
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestBuildSpec_AssigneeDispatchOverridesGlobalVars(t *testing.T) {
	d := newMinimalDispatcher(t)
	cfg := &Config{
		Workflow: "/tmp/default.bot",
		Dispatch: DispatchConfig{
			Vars: map[string]string{
				"global_var": "global={{issue.title}}",
			},
		},
		AssigneeWorkflows: map[string]string{
			"feature-bot": "/tmp/feature.bot",
		},
		AssigneeDispatch: map[string]DispatchConfig{
			"feature-bot": {Vars: map[string]string{
				"feature_prompt": "{{issue.title}}\n\n{{issue.body}}",
				"workspace_dir":  "{{dispatcher.workspace_path}}",
			}},
		},
	}
	iss := tracker.Issue{
		ID:         "i-1",
		Identifier: "I-1",
		Title:      "Add dark mode",
		Body:       "Toggle in settings panel.",
		Assignee:   "feature-bot",
	}
	spec := d.buildSpec(cfg, iss, "run-1", "/tmp/ws/i-1", 0, nil)
	want := map[string]any{
		"feature_prompt": "Add dark mode\n\nToggle in settings panel.",
		"workspace_dir":  "/tmp/ws/i-1",
	}
	if !reflect.DeepEqual(spec.Vars, want) {
		t.Fatalf("vars mismatch:\n got %#v\nwant %#v", spec.Vars, want)
	}
	if _, ok := spec.Vars["global_var"]; ok {
		t.Fatalf("global var leaked into per-assignee dispatch: %v", spec.Vars)
	}
}

func TestBuildSpec_FallsBackToGlobalDispatchWhenNoOverride(t *testing.T) {
	d := newMinimalDispatcher(t)
	cfg := &Config{
		Workflow: "/tmp/default.bot",
		Dispatch: DispatchConfig{
			Vars: map[string]string{
				"issue_title": "{{issue.title}}",
			},
		},
		// No AssigneeDispatch entry for "anon-bot".
		AssigneeWorkflows: map[string]string{
			"anon-bot": "/tmp/anon.bot",
		},
	}
	iss := tracker.Issue{ID: "i-2", Title: "Question", Assignee: "anon-bot"}
	spec := d.buildSpec(cfg, iss, "run-2", "/tmp/ws/i-2", 0, nil)
	if spec.Vars["issue_title"] != "Question" {
		t.Fatalf("expected fallback global var to render, got %v", spec.Vars)
	}
}

func TestBuildSpec_NoAssigneeUsesGlobalDispatch(t *testing.T) {
	d := newMinimalDispatcher(t)
	cfg := &Config{
		Workflow: "/tmp/default.bot",
		Dispatch: DispatchConfig{
			Vars: map[string]string{"issue_body": "{{issue.body}}"},
		},
		AssigneeDispatch: map[string]DispatchConfig{
			"someone": {Vars: map[string]string{"x": "would not apply"}},
		},
	}
	iss := tracker.Issue{ID: "i-3", Body: "no one assigned"}
	spec := d.buildSpec(cfg, iss, "run-3", "/tmp/ws/i-3", 0, nil)
	if spec.Vars["issue_body"] != "no one assigned" {
		t.Fatalf("expected global fallback when Assignee empty: %v", spec.Vars)
	}
	if _, ok := spec.Vars["x"]; ok {
		t.Fatalf("per-assignee leaked when Assignee empty: %v", spec.Vars)
	}
}

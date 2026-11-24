package runner

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestExecutorSpecNamesTheLaunchRowsOffTheRunDocument pins the cloud half of
// the effort's provenance: the pod's executor is told which of the run's
// override rows the launch itself carried, read off the run's document — for
// a subbot child, whose document its engine has not created yet, off its
// parent's — so a node effort a routing cell set reads `cell` and one the
// launch set reads `launch`. A run no document answers for names neither.
//
// Mutation that reddens it: drop LaunchModelOverrides from executorSpec, or
// the parent fallback of launchOverridesFor.
func TestExecutorSpecNamesTheLaunchRowsOffTheRunDocument(t *testing.T) {
	t.Parallel()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	launch := []store.RunModelOverride{{Selector: "review", Effort: "low"}}
	if _, err := st.CreateRun(ctx, "root", "wf", nil); err != nil {
		t.Fatal(err)
	}
	root, err := st.LoadRun(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	root.LaunchModelOverrides = launch
	root.ModelOverrides = append(slicesOf(launch), store.RunModelOverride{Selector: "other", Effort: "high"})
	root.LLMRoutePolicy = &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{{Selector: "*/other", Effort: "high"}}}
	if err := st.SaveRun(ctx, root); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	wf := &ir.Workflow{Name: "wf", Nodes: map[string]ir.Node{}}
	for _, tc := range []struct {
		name  string
		msg   *queue.RunMessage
		known bool
	}{
		{"the root run", &queue.RunMessage{RunID: "root", WorkflowName: "wf"}, true},
		{"a subbot child before its document", &queue.RunMessage{RunID: "child", ParentRunID: "root", WorkflowName: "wf"}, true},
		{"a run no document answers for", &queue.RunMessage{RunID: "ghost", WorkflowName: "wf"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, _, err := r.executorSpec(ctx, tc.msg, wf, "", iterlog.Nop(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if (spec.LaunchModelOverrides != nil) != tc.known {
				t.Fatalf("launch rows known = %v, want %v", spec.LaunchModelOverrides != nil, tc.known)
			}
			if !tc.known {
				return
			}
			if got := spec.LaunchModelOverrides.ForNode("review", ir.NodeAgent).Effort; got != "low" {
				t.Errorf("the launch rows give review effort %q, want low", got)
			}
			if got := spec.LaunchModelOverrides.ForNode("other", ir.NodeAgent).Effort; got != "" {
				t.Errorf("the launch rows give other effort %q — a routing cell's, not the launch's", got)
			}
		})
	}
}

func slicesOf(rows []store.RunModelOverride) []store.RunModelOverride {
	return append([]store.RunModelOverride(nil), rows...)
}

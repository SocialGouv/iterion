package runview

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestLaunchOverridesOfRunReadsWhatTheDocumentSays pins the reading of a run
// document's launch rows the executor names an effort's source from, and the
// resume path that hands them to it: the recorded rows; a document with no
// policy cell, whose overrides are all the launch's; a document that recorded
// none beside policy cells, which holds no launch row.
//
// Mutation that reddens it: return run.ModelOverrides whatever the policy, or
// drop LaunchModelOverrides from resumeExecutorSpec.
func TestLaunchOverridesOfRunReadsWhatTheDocumentSays(t *testing.T) {
	cells := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{{Selector: "*/review", Effort: "high"}}}
	for _, tc := range []struct {
		name string
		run  store.Run
		want string // the review node's effort among the launch rows
	}{
		{"recorded rows", store.Run{LaunchModelOverrides: []store.RunModelOverride{{Selector: "review", Effort: "low"}}, ModelOverrides: []store.RunModelOverride{{Selector: "review", Effort: "low"}}, LLMRoutePolicy: cells}, "low"},
		{"no policy cell: the overrides are the launch's", store.Run{ModelOverrides: []store.RunModelOverride{{Selector: "review", Effort: "xhigh"}}}, "xhigh"},
		{"policy cells and no recorded row", store.Run{ModelOverrides: []store.RunModelOverride{{Selector: "review", Effort: "high"}}, LLMRoutePolicy: cells}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := LaunchOverridesOfRun(&tc.run).ForNode("review", ir.NodeAgent).Effort
			if got != tc.want {
				t.Errorf("launch rows give review %q, want %q", got, tc.want)
			}
			s := &Service{}
			spec := s.resumeExecutorSpec(&ir.Workflow{Name: "wf"}, &tc.run, iterlog.Nop(), "")
			if spec.LaunchModelOverrides == nil {
				t.Fatal("the resume spec does not carry the launch rows")
			}
			if got := spec.LaunchModelOverrides.ForNode("review", ir.NodeAgent).Effort; got != tc.want {
				t.Errorf("resume spec launch rows give review %q, want %q", got, tc.want)
			}
		})
	}
}

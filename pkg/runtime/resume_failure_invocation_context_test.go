package runtime

import (
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

// This is an opposite for the second constructor: current failure resumes reach
// execLoop before edge emission, so it does not pretend that removing the new
// early assignment necessarily changes today's externally visible behavior.
func TestResumeFromFailurePreservesInvocationContext(t *testing.T) {
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			base, ctx, cancel, deadline := newResumeInvocationFixture(t, f)
			if err := base.FailRunResumable(ctx, "reviewer-resume", &store.Checkpoint{NodeID: "step"}, "retry", ""); err != nil {
				t.Fatal(err)
			}
			wf := minimalReviewWorkflow()
			wf.Entry = "step"
			wf.Nodes["step"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step"}}
			wf.Edges = []*ir.Edge{{From: "step", To: "done"}}
			s := &resumeInvocationEventStore{RunStore: base, t: t, format: f, deadline: deadline, mode: "normal", cancel: cancel}
			e := New(wf, s, newStubExecutor(), WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
			err := e.Resume(ctx, "reviewer-resume", nil)
			if err != nil || s.edges != 1 || len(s.missing) > 0 {
				t.Fatalf("FAILURE_RESUME_CONTEXT: err=%v edges=%d missing=%v", err, s.edges, s.missing)
			}
		})
	}
}

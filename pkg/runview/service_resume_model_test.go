package runview

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A resume gets no LaunchSpec, so every launch-time decision has to come back
// off the run document. This one used to be dropped: the resume executor was
// built with no ModelOverrides at all, so the run silently fell back to the
// .bot's own model:/backend:/reasoning_effort: from the first resume onward
// while the studio header kept displaying the operator's choice.
//
// It hurts most where it shows least. A conversational run pauses on its chat
// node and EVERY operator reply is a resume — so "the model I picked" held for
// exactly one turn, and the operator was billed against a provider they had
// deliberately steered away from.
func TestResumeExecutorSpecReplaysTheLaunchedModelChoice(t *testing.T) {
	s := &Service{storeDir: t.TempDir()}
	r := &store.Run{
		ID: "run-1",
		ModelOverrides: []store.RunModelOverride{
			{Selector: "*", Model: "openai/gpt-5.5", Backend: "claw", Effort: "high"},
			{Selector: "judge", Provider: "anthropic"},
		},
	}

	spec := s.resumeExecutorSpec(&ir.Workflow{Name: "wf"}, r, nil)

	if spec.RunID != "run-1" {
		t.Fatalf("RunID = %q, want run-1", spec.RunID)
	}
	if spec.ModelOverrides.Empty() {
		t.Fatal("resume rebuilt the executor with no model overrides — the run would silently revert to the .bot's own model")
	}
	got := spec.ModelOverrides.ForNode("chat", ir.NodeAgent)
	if got.Model != "openai/gpt-5.5" || got.Backend != "claw" || got.Effort != "high" {
		t.Errorf("ForNode(chat) = %+v, want the launch's model/backend/effort", got)
	}
	if j := spec.ModelOverrides.ForNode("judge", ir.NodeJudge); j.Provider != "anthropic" {
		t.Errorf("ForNode(judge).Provider = %q, want anthropic", j.Provider)
	}
}

// A run launched without a choice must resume without one, so the executor's
// "empty means inherit the DSL" path keeps its meaning.
func TestResumeExecutorSpecStaysEmptyWithoutALaunchChoice(t *testing.T) {
	s := &Service{storeDir: t.TempDir()}

	spec := s.resumeExecutorSpec(&ir.Workflow{Name: "wf"}, &store.Run{ID: "run-2"}, nil)

	if !spec.ModelOverrides.Empty() {
		t.Errorf("ModelOverrides = %+v, want empty", spec.ModelOverrides)
	}
}

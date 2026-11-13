package model

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// interactionWorkflow is an agent whose `interaction: llm` answers the
// delegate's questions with a DIRECT generation on its own interaction
// model — a second route the node spends, on a credential its own route
// does not name (planBundle's own openai key beside its anthropic one).
func interactionWorkflow() *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{
		"ask": &ir.AgentNode{
			BaseNode:          ir.BaseNode{ID: "ask"},
			LLMFields:         ir.LLMFields{Provider: "anthropic", Model: "anthropic/claude-opus-5"},
			InteractionFields: ir.InteractionFields{Interaction: ir.InteractionLLM, InteractionModel: "openai/gpt-5"},
		},
	}}
}

// The interaction generation runs under a synthetic human node,
// `<node>_interaction` (ExecuteHumanLLMForInteraction), and every event it
// fires carries THAT id — the asking node never spends a token of it under
// its own. The plan keys the walk's `ask#interaction` site a second time
// under the runtime id, or every fired interaction joins no row: a miss that
// voids the run's fine table. The node-half row stays (an authored fallback
// named "interaction" spells the same site); the runtime-id row is the one
// the runner's join reads. Red when runtimePlanNode drops the interaction
// case.
func TestCredentialPlan_theInteractionGenerationPlansUnderTheRuntimeID(t *testing.T) {
	t.Parallel()

	res := EffectiveProviders(RouteView{Workflow: interactionWorkflow()}, knownForTest)
	if !res.NarrowSafe {
		t.Fatalf("the interaction walk widened — test premise broken: %+v", res)
	}
	if got := res.Sites["ask#interaction"]; !slices.Equal(got, []string{"openai"}) {
		t.Fatalf("Sites[ask#interaction] = %v, want [openai] — the walk reads the generation at its site", got)
	}
	plan := CredentialPlan(res, planBundle())
	want := map[string]string{
		"ask|anthropic":          "fp-own-anthropic",
		"ask|openai":             "fp-own-openai",
		"ask_interaction|openai": "fp-own-openai",
	}
	if !maps.Equal(plan, want) {
		t.Fatalf("plan = %v, want %v — the interaction keys a second row under the runtime id its generation's events carry", plan, want)
	}
}

// The runtime half of the same join, driven through the production entry:
// the generation ExecuteHumanLLMForInteraction runs stamps each step with
// the synthetic node's id and the loop's provider word — the (node,
// provider) pair the runner's join reads off llm_step_finished — and the
// plan the walk derives for the asking node must hold that exact key,
// naming the interaction model's own credential. Red when the runtime id
// and the plan's runtime-id row part (either side spelling its own id),
// when the plan keys the generation on the asking node alone, or when the
// generation stops stamping its word (the key's provider half goes empty).
func TestExecuteHumanLLMForInteraction_stepsJoinThePlansRuntimeRow(t *testing.T) {
	t.Parallel()

	plan := CredentialPlan(EffectiveProviders(RouteView{Workflow: interactionWorkflow()}, knownForTest), planBundle())

	client := newMockClient(toolUseEvents("answer", "structured_output", `{"which_db":"postgres"}`, 70, 30))
	reg := NewRegistry()
	reg.Register("openai", func(string) (api.APIClient, error) { return client, nil })
	type observed struct {
		node string
		step LLMStepInfo
	}
	var steps []observed
	exec := NewClawExecutor(reg, interactionWorkflow(), WithEventHooks(EventHooks{
		OnLLMStepFinish: func(node string, step LLMStepInfo) { steps = append(steps, observed{node, step}) },
	}))
	ask := interactionWorkflow().Nodes["ask"].(*ir.AgentNode)
	if _, _, _, err := exec.ExecuteHumanLLMForInteraction(context.Background(), "ask",
		&ErrNeedsInteraction{Questions: map[string]any{"which_db": "which database?"}}, ask.InteractionFields); err != nil {
		t.Fatalf("the interaction generation failed: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("the interaction generation fired no step — the witness premise is broken")
	}
	for _, o := range steps {
		key := o.node + PlanKeySep + o.step.Provider
		if got := plan[key]; got != "fp-own-openai" {
			t.Fatalf("the interaction's step joins %q, which the plan maps to %q (plan %v) — want the interaction model's own credential fp-own-openai", key, got, plan)
		}
	}
}

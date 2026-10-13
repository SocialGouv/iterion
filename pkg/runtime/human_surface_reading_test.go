package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// reviewCompanionExecutor answers as the review companion and records what
// the runtime handed it.
type reviewCompanionExecutor struct {
	*stubExecutor
	system string
	user   string
}

func (c *reviewCompanionExecutor) ExecuteReviewCompanion(_ context.Context, _ *ir.HumanNode, systemText, userMessage string) (map[string]any, error) {
	c.system, c.user = systemText, userMessage
	return map[string]any{"needs_human_input": true, "message": "walk through it"}, nil
}

// reviewGateWorkflow is a review gate whose input carries two keys, one
// rendered by its system prompt and one named in prose only, plus a
// `review_url` reading a key no route maps.
func reviewGateWorkflow(t *testing.T) (*ir.Workflow, *ir.HumanNode) {
	t.Helper()
	const (
		systemBody = "You review. input.dead is the change; rendered [{{input.shown}}]; absent [{{input.absent}}]"
		instrBody  = "Operator: read input.dead; rendered [{{input.shown}}]; absent [{{input.absent}}]"
		urlBody    = "https://review.example/{{input.absent}}"
	)
	refs := func(body string) []*ir.Ref {
		parsed, err := ir.ParseRefs(body)
		if err != nil {
			t.Fatalf("parse %q: %v", body, err)
		}
		return parsed
	}
	gate := &ir.HumanNode{
		BaseNode:          ir.BaseNode{ID: "gate"},
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionReview},
		Model:             "anthropic/claude-sonnet-4-6",
		SystemPrompt:      "gate_system",
		Instructions:      "gate_instructions",
		ReviewURL:         urlBody,
		ReviewURLRefs:     refs(urlBody),
	}
	gate.InputSchema = "gate_in"
	gate.OutputSchema = "gate_verdict"
	wf := &ir.Workflow{
		Name:  "review_gate_reading",
		Entry: "seed",
		Nodes: map[string]ir.Node{
			"seed": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "seed"}},
			"gate": gate,
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "seed", To: "gate", With: []*ir.DataMapping{
				{Key: "dead", Raw: "CARRIED-DEAD"},
				{Key: "shown", Raw: "CARRIED-SHOWN"},
			}},
			{From: "gate", To: "done"},
		},
		Prompts: map[string]*ir.Prompt{
			"gate_system":       {Name: "gate_system", Body: systemBody, TemplateRefs: refs(systemBody)},
			"gate_instructions": {Name: "gate_instructions", Body: instrBody, TemplateRefs: refs(instrBody)},
		},
		Schemas: map[string]*ir.Schema{
			"gate_in": {Name: "gate_in", Fields: []*ir.SchemaField{
				{Name: "dead", Type: ir.FieldTypeString},
				{Name: "shown", Type: ir.FieldTypeString},
			}},
			"gate_verdict": {Name: "gate_verdict", Fields: []*ir.SchemaField{{Name: "decision", Type: ir.FieldTypeString}}},
		},
		Vars:  map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{},
	}
	return wf, gate
}

// reviewGateState wires an engine on that workflow with the source node's
// output in place, so the gate's incoming edge contributes its mappings.
func reviewGateState(t *testing.T, exec NodeExecutor) (*Engine, *runState, *ir.HumanNode) {
	t.Helper()
	wf, gate := reviewGateWorkflow(t)
	ctx := context.Background()
	s := tmpStore(t)
	if _, err := s.CreateRun(ctx, "run-review-reading", wf.Name, nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	eng := New(wf, s, exec)
	rs := eng.newRunState("run-review-reading", nil)
	rs.ctx = ctx
	rs.outputs["seed"] = map[string]any{"payload": "p"}
	return eng, rs, gate
}

// A review gate's companion reads the node's input through its SYSTEM
// PROMPT alone: the engine hands it the change under review and the
// dialogue as the user message (buildCompanionMessage), never the input. So
// a key the input carries that the system prompt does not render is on no
// screen of that reader — which is why the catalogue prose guard keeps the
// strict reading on this one surface
// (bots/prompt_prose_input_refs_test.go, humanReaderGetsWholeInput), while
// crediting the whole input on every other human surface.
func TestAReviewCompanionReadsItsInputThroughItsSystemPromptAlone(t *testing.T) {
	companion := &reviewCompanionExecutor{stubExecutor: newStubExecutor()}
	eng, rs, gate := reviewGateState(t, companion)

	out := eng.runReviewCompanion(rs.ctx, rs, gate, "gate", nil)
	if out["needs_human_input"] != true {
		t.Fatalf("companion output = %v, want the pause verdict", out)
	}

	// The input really holds both keys: what follows is about screens, not
	// about plumbing.
	questions := eng.buildNodeInputRS("gate", rs.scope())
	if questions["dead"] != "CARRIED-DEAD" || questions["shown"] != "CARRIED-SHOWN" {
		t.Fatalf("the gate's input = %v, want both mapped keys", questions)
	}

	if strings.Contains(companion.user, "CARRIED-") {
		t.Errorf("the companion's user message carries the node input (%q) — this test's premise, and the guard's, is that it carries the change under review instead", companion.user)
	}
	if !strings.Contains(companion.user, "Begin the review") {
		t.Errorf("the companion's user message = %q, want the review kickoff", companion.user)
	}
	if !strings.Contains(companion.system, "CARRIED-SHOWN") {
		t.Errorf("the companion's system prompt = %q, want the rendered key in it", companion.system)
	}
	if strings.Contains(companion.system, "CARRIED-DEAD") {
		t.Errorf("the companion's system prompt = %q, want the unrendered key absent from it", companion.system)
	}
}

// The operator-facing texts of a human node are rendered by
// Engine.renderHumanInstructions, which substitutes the EMPTY STRING for a
// key the paused node's input does not carry: the instructions, the
// `review_url`, and a review gate's system prompt, which the same function
// serves. A model's prompt goes the other way — model.TemplateResolver
// keeps the unresolvable reference as written — so the same authoring
// mistake is loud there and silent here. Both readings are what the input
// guards report (bots/catalog_input_mapping_test.go, absentReadDelivers).
func TestHumanInstructionsDropAnAbsentInputToTheEmptyString(t *testing.T) {
	eng, rs, gate := reviewGateState(t, newStubExecutor())
	questions := eng.buildNodeInputRS("gate", rs.scope())

	instructions := eng.renderHumanInstructions(eng.workflow.Prompts[gate.Instructions], questions, rs)
	if want := "Operator: read input.dead; rendered [CARRIED-SHOWN]; absent []"; instructions != want {
		t.Errorf("instructions = %q, want %q — an absent key must render as the empty string", instructions, want)
	}

	system := eng.renderHumanInstructions(eng.workflow.Prompts[gate.SystemPrompt], questions, rs)
	if !strings.Contains(system, "absent []") {
		t.Errorf("a review gate's system prompt = %q, want the absent key rendered as the empty string", system)
	}

	if url := eng.resolveReviewURL(gate, questions, rs); url != "https://review.example/" {
		t.Errorf("review_url = %q, want the absent key rendered as the empty string", url)
	}

	// The contrast: the same body on a model's prompt keeps the reference.
	onPrompt := (&model.TemplateResolver{}).Resolve("absent [{{input.absent}}]", questions, nil)
	if want := "absent [{{input.absent}}]"; onPrompt != want {
		t.Errorf("a model's prompt rendered %q, want %q — the two renderers must stay distinguishable", onPrompt, want)
	}
}

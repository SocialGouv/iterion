package model

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// An `interaction_prompt:` is the system prompt of the stand-in model that
// answers a node's QUESTIONS, and its input is the question map the asking
// node produced — ExecuteHumanLLMForInteraction builds a synthetic input
// from ni.Questions, never the input the incoming edges mapped. So
// `{{input.<question_key>}}` is the spelling that renders there, and a key
// that is not a question stays the literal placeholder. The compiler relies
// on this to keep quiet on that surface (pkg/dsl/ir, rc.OnQuestions): C034
// used to REFUSE the only spelling a question key has, and C308 prescribed a
// `with` mapping the text never reads.
func TestAnInteractionPromptRendersTheQuestionsNotTheNodesInput(t *testing.T) {
	reg := NewRegistry()
	mock := &captureRequestClient{stream: mockStreamEvents(`{"q1":"4"}`, "end_turn")}
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })

	wf := &ir.Workflow{
		Name:  "interaction_scope",
		Nodes: map[string]ir.Node{},
		Prompts: map[string]*ir.Prompt{
			"ask": {Name: "ask", Body: "Question: {{input.q1}} / inventory: {{input.inv}}"},
		},
		Schemas: map[string]*ir.Schema{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	var systemText, userText string
	e := NewClawExecutor(reg, wf, WithEventHooks(EventHooks{
		OnLLMPrompt: func(_, system, user string) { systemText, userText = system, user },
	}))

	// The generation itself ends on the mock's stream; the prompt is built
	// and reported before the call, which is what this pins.
	_, _, _ = e.ExecuteHumanLLMForInteraction(context.Background(), "worker",
		&ErrNeedsInteraction{NodeID: "worker", Questions: map[string]any{"q1": "How many ADRs?"}},
		ir.InteractionFields{Interaction: ir.InteractionLLM, InteractionPrompt: "ask", InteractionModel: "anthropic/claude-sonnet-4-6"})

	if !strings.Contains(systemText, "Question: How many ADRs?") {
		t.Errorf("the interaction prompt rendered %q — a question key must resolve from the question map", systemText)
	}
	if !strings.Contains(systemText, "inventory: {{input.inv}}") {
		t.Errorf("the interaction prompt rendered %q — a key that is NOT a question must stay the literal placeholder, since no edge mapping can reach this surface", systemText)
	}
	if !strings.Contains(userText, "How many ADRs?") {
		t.Errorf("the interaction user message is %q, want the questions it has to answer", userText)
	}
}

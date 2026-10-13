package model

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A human node has no user-prompt field at all, so the stand-in model of an
// `interaction: llm` / `llm_or_human` gate is handed the node's WHOLE input
// as its user message, serialized — whatever its system prompt renders
// (executeHumanLLM: buildUserMessage("", input, td)). The catalogue prose
// guard judges that surface on this fact
// (bots/prompt_prose_input_refs_test.go, humanReaderGetsWholeInput), so a
// user message built from a prompt instead would turn a true verdict there
// into a false one.
func TestHumanLLMUserMessageIsTheWholeInput(t *testing.T) {
	reg := NewRegistry()
	mock := &captureRequestClient{stream: mockStreamEvents(`{"approve":true}`, "end_turn")}
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })

	node := &ir.HumanNode{
		BaseNode:          ir.BaseNode{ID: "gate"},
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionLLMOrHuman},
		Model:             "anthropic/claude-sonnet-4-6",
		SystemPrompt:      "gate_sys",
	}
	node.OutputSchema = "decision"
	wf := &ir.Workflow{
		Name:  "human_whole_input",
		Nodes: map[string]ir.Node{"gate": node},
		Prompts: map[string]*ir.Prompt{
			// Names the field in prose only: nothing here renders it.
			"gate_sys": {Name: "gate_sys", Body: "You stand in for the human. input.ctx is the context."},
		},
		Schemas: map[string]*ir.Schema{
			"decision": {Name: "decision", Fields: []*ir.SchemaField{{Name: "approve", Type: ir.FieldTypeBool}}},
		},
		Vars:  map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{},
	}

	var userText string
	e := NewClawExecutor(reg, wf, WithEventHooks(EventHooks{
		OnLLMPrompt: func(_, _, user string) { userText = user },
	}))

	// The generation itself fails on the mock's text-only stream; the prompt
	// is built and reported before the call, which is what this pins.
	_, _ = e.executeHumanLLM(context.Background(), node, map[string]any{"ctx": "THE-CONTEXT", "other": "ALSO"}, nil)

	for _, want := range []string{"THE-CONTEXT", "ALSO"} {
		if !strings.Contains(userText, want) {
			t.Errorf("the user message %q does not carry %q — a human node's input must reach the stand-in model whole", userText, want)
		}
	}
	if len(mock.reqs) == 0 {
		t.Fatal("no request reached the client")
	}
	var wire string
	for _, m := range mock.last().Messages {
		if m.Role != "user" {
			continue
		}
		for _, c := range m.Content {
			wire += c.Text
		}
	}
	if !strings.Contains(wire, "THE-CONTEXT") {
		t.Errorf("the request's user message is %q — the input did not reach the wire", wire)
	}
}

package bots

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestWhatsNextEmptySeedOpensWithoutRewritingMessages(t *testing.T) {
	w := compileBot(t, "whats-next")
	if w == nil {
		t.Fatal("whats-next does not compile")
	}
	node, ok := w.Nodes["nexie"].(ir.LLMNode)
	if !ok {
		t.Fatal("nexie is not an LLM node")
	}
	prompt := w.Prompts[node.GetLLMFields().UserPrompt]
	if prompt == nil {
		t.Fatal("nexie has no user prompt")
	}
	seed, ok := w.Nodes["seed"].(*ir.ComputeNode)
	if !ok {
		t.Fatal("seed is not a compute node")
	}
	initial, ok := w.Vars["initial_message"]
	if !ok || !initial.HasDefault || initial.Default != "" {
		t.Fatal("the bare launch no longer defaults to an empty seed")
	}
	for _, tt := range []struct {
		name    string
		message string
	}{
		{"empty seed", ""},
		{"whitespace seed", " \n\t "},
		{"seeded launch", "Review the board before recommending a ticket."},
		{"slash command", "/probe-secret argument"},
		{"later turn", "Keep working on #42.\nDo not dispatch anything."},
		{"literal template", "Explain {{vars.scope_notes}} without replacing it."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := make(map[string]any)
			ctx := &expr.Context{Vars: func(path []string) any {
				if len(path) != 1 || path[0] != "initial_message" {
					t.Fatalf("unexpected seed reference: %v", path)
				}
				return tt.message
			}}
			for _, field := range seed.Exprs {
				value, err := field.AST.Eval(ctx)
				if err != nil {
					t.Fatalf("seed %s: %v", field.Key, err)
				}
				input[field.Key] = value
			}
			resolver := &model.TemplateResolver{Unresolved: func(ref string) {
				t.Errorf("unresolved prompt reference: %s", ref)
			}}
			got := resolver.Resolve(prompt.Body, input, nil)
			if tt.message == "" && strings.TrimSpace(got) == "" {
				t.Fatal("nexie sends an empty user prompt to the delegate")
			}
			if tt.message != "" && got != tt.message {
				t.Fatalf("operator message changes: got %q, want %q", got, tt.message)
			}
			if tt.message != "" {
				// Chat replies enter nexie directly, without revisiting seed.
				got = resolver.Resolve(prompt.Body, map[string]any{"operator_message": tt.message}, nil)
				if got != tt.message {
					t.Fatalf("chat reply changes: got %q, want %q", got, tt.message)
				}
			}
		})
	}
}

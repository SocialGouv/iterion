package bots

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// evolve's advisory `both_approved` reads "no participating reviewer
// objected". The roles decide who participates (ADR-169): `reviewer` runs on
// every round and fills the claude_* fields of aggregate_review's input;
// `reviewer_alternate` runs only on a paired round and fills the gpt_* ones.
// A single round therefore leaves gpt_* ABSENT, and absent must read as "did
// not object" — otherwise the default topology could never approve.
//
// Evaluated on the expression the shipped bot compiles, with each edge's
// fields present only where that edge fires, so a guard keyed on the wrong
// role reddens here.
func TestEvolveBothApprovedFollowsTheRolesThatRan(t *testing.T) {
	cr := ir.Compile(parseBotUnit("evolve/main.bot").File)
	if cr.Workflow == nil {
		t.Fatal("bots/evolve/main.bot does not compile")
	}
	node, ok := cr.Workflow.Nodes["aggregate_review"].(*ir.ComputeNode)
	if !ok {
		t.Fatalf("aggregate_review is %T, want a compute node", cr.Workflow.Nodes["aggregate_review"])
	}
	var ast *expr.AST
	for _, e := range node.Exprs {
		if e.Key == "both_approved" {
			ast = e.AST
		}
	}
	if ast == nil {
		t.Fatal("aggregate_review computes no both_approved")
	}
	for _, tc := range []struct {
		name  string
		mode  string
		input map[string]any
		want  bool
	}{
		{"single, the reviewer approves", "single", map[string]any{"claude_approved": true}, true},
		{"single, the reviewer objects", "single", map[string]any{"claude_approved": false}, false},
		{"paired, both approve", "paired", map[string]any{"claude_approved": true, "gpt_approved": true}, true},
		{"paired, the alternate objects", "paired", map[string]any{"claude_approved": true, "gpt_approved": false}, false},
		{"paired, the reviewer objects", "paired", map[string]any{"claude_approved": false, "gpt_approved": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ast.Eval(&expr.Context{
				Vars: func(path []string) any {
					if len(path) == 1 && path[0] == "review_mode" {
						return tc.mode
					}
					return nil
				},
				Input: func(path []string) any {
					if len(path) == 1 {
						return tc.input[path[0]]
					}
					return nil
				},
			})
			if err != nil {
				t.Fatalf("both_approved fails to evaluate on %v: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("both_approved = %v on a %s round with %v, want %v", got, tc.mode, tc.input, tc.want)
			}
		})
	}
}

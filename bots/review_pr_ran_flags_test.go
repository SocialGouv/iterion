package bots

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A role RAN when its node produced an output, never when the model named
// its vendor. A routing cell can re-target either role to any family, and a
// model answers its own `family`: a `ran` flag keyed on the declared word
// would read false for a role that reviewed, and the merge would be told no
// reviewer ran — approval by omission on this repo's own required gate.
func TestReviewPrRanFlagsReadTheOutputNotTheVendor(t *testing.T) {
	cr := ir.Compile(parseBotUnit("review-pr/main.bot").File)
	if cr.Workflow == nil {
		t.Fatal("review-pr does not compile")
	}
	exprs := map[string]*expr.AST{}
	for _, e := range cr.Workflow.Nodes["merge_reviews"].(*ir.ComputeNode).Exprs {
		exprs[e.Key] = e.AST
	}
	eval := func(t *testing.T, key string, outputs map[string]map[string]any) any {
		t.Helper()
		ast := exprs[key]
		if ast == nil {
			t.Fatalf("merge_reviews computes no %s", key)
		}
		got, err := ast.Eval(&expr.Context{Outputs: func(path []string) any {
			if len(path) == 2 {
				return outputs[path[0]][path[1]]
			}
			return nil
		}})
		if err != nil {
			t.Fatalf("%s fails to evaluate: %v", key, err)
		}
		return got
	}
	for _, tc := range []struct {
		name    string
		key     string
		outputs map[string]map[string]any
		want    bool
	}{
		{"the alternate re-targeted to a gpt model still ran", "claude_ran", map[string]map[string]any{"reviewer_alternate": {"family": "gpt"}}, true},
		{"the alternate absent did not run", "claude_ran", map[string]map[string]any{}, false},
		{"the reviewer re-targeted to a claude model still ran", "gpt_ran", map[string]map[string]any{"reviewer": {"family": "claude"}}, true},
		{"the glance reviewer re-targeted still ran", "gpt_ran", map[string]map[string]any{"reviewer_glance": {"family": "claude"}}, true},
		{"no reviewer did not run", "gpt_ran", map[string]map[string]any{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eval(t, tc.key, tc.outputs); got != tc.want {
				t.Fatalf("%s = %v on %v, want %v", tc.key, got, tc.outputs, tc.want)
			}
		})
	}
}

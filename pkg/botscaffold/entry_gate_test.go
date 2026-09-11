package botscaffold

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// configuredExpr returns the `configured` expression of a shape's `check`
// compute — the entry gate that refuses an unfilled placeholder.
func configuredExpr(t *testing.T, shape string) *expr.AST {
	t.Helper()
	return gateExpr(t, shape, "check", "configured")
}

// gateExpr returns one expression of a shape's compute `node` by key.
func gateExpr(t *testing.T, shape, node, key string) *expr.AST {
	t.Helper()
	tpl, ok := TemplateByID(shape)
	if !ok {
		t.Fatalf("no %s template", shape)
	}
	spec := tpl.Spec
	spec.Slug = "gate"
	_, w, _ := scaffoldAndCompile(t, spec)
	for _, c := range nodesOf[*ir.ComputeNode](w) {
		if c.ID != node {
			continue
		}
		for _, e := range c.Exprs {
			if e.Key == key {
				return e.AST
			}
		}
	}
	t.Fatalf("%s: no `%s` expression on compute %s", shape, key, node)
	return nil
}

// evalGate evaluates a gate expression the way the runtime does, against
// the vars the run resolved (an int var reaches the evaluator as int64, a
// string var as string, an undeclared one as nil).
func evalGate(t *testing.T, ast *expr.AST, vars map[string]any) (any, error) {
	t.Helper()
	return ast.Eval(&expr.Context{Vars: func(path []string) any {
		if len(path) != 1 {
			return nil
		}
		return vars[path[0]]
	}})
}

// TestEntryGatesRefuseAnUnfilledPlaceholder runs the shipped gate
// expressions through the real evaluator. What must be refused: the empty
// default, a BLANK value (`!= ”` admitted one), an ABSENT var (nil is not
// "" either), and a pass bound under 1. A non-numeric bound does not reach
// the typed refusal — the resolver keeps a bad override as text and the
// comparison errors the node loudly instead; that is pinned as an error,
// not as an admission.
func TestEntryGatesRefuseAnUnfilledPlaceholder(t *testing.T) {
	campaign := configuredExpr(t, "campaign-loop")
	for _, c := range []struct {
		name string
		vars map[string]any
		want bool
	}{
		{"empty default", map[string]any{"verify_command": "", "max_passes": int64(4)}, false},
		{"blank verifier", map[string]any{"verify_command": "  \t", "max_passes": int64(4)}, false},
		{"absent verifier", map[string]any{"max_passes": int64(4)}, false},
		{"real verifier", map[string]any{"verify_command": "go test ./...", "max_passes": int64(4)}, true},
		{"zero passes", map[string]any{"verify_command": "go test ./...", "max_passes": int64(0)}, false},
		{"negative passes", map[string]any{"verify_command": "go test ./...", "max_passes": int64(-3)}, false},
		{"one pass", map[string]any{"verify_command": "make check", "max_passes": int64(1)}, true},
	} {
		got, err := evalGate(t, campaign, c.vars)
		if err != nil {
			t.Errorf("campaign-loop %s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("campaign-loop %s: configured = %v, want %v", c.name, got, c.want)
		}
	}
	if _, err := evalGate(t, campaign, map[string]any{"verify_command": "go test ./...", "max_passes": "lots"}); err == nil {
		t.Errorf("campaign-loop: a non-numeric bound evaluated instead of failing the gate loudly")
	}
	// The loop cap the GATE derives on every pass (never the entry, which a
	// resume does not re-run): `as passes(N)` allows N re-entries, so
	// max_passes PASSES are max_passes-1 crossings.
	if got, err := evalGate(t, gateExpr(t, "campaign-loop", "gate", "passes_after_first"), map[string]any{"verify_command": "make check", "max_passes": int64(4)}); err != nil || got != int64(3) {
		t.Errorf("campaign-loop passes_after_first(4) = %v, %v; want 3", got, err)
	}

	action := configuredExpr(t, "verified-action")
	for _, c := range []struct {
		name string
		vars map[string]any
		want bool
	}{
		{"empty default", map[string]any{"tag": ""}, false},
		{"blank tag", map[string]any{"tag": " "}, false},
		{"absent tag", map[string]any{}, false},
		{"a tag", map[string]any{"tag": "v1.2.3"}, true},
	} {
		got, err := evalGate(t, action, c.vars)
		if err != nil {
			t.Errorf("verified-action %s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("verified-action %s: configured = %v, want %v", c.name, got, c.want)
		}
	}
}

package runtime

import (
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"testing"
)

// An unexecuted fan-out route has no output entry. Preserve nil (an empty
// collection to concat), rather than returning a typed nil map as its value.
func TestGroupOutputResolverKeepsMissingOutputsNil(t *testing.T) {
	e := New(&ir.Workflow{}, tmpStore(t), newStubExecutor())
	rs := e.newRunState("missing-output", nil)
	rs.outputs["r1.present"] = map[string]any{"items": []any{"ok"}}
	for _, src := range []string{"outputs.absent.items", "outputs.r1.absent.items", "outputs.absent"} {
		value, err := expr.MustParse(src).Eval(e.exprContext(rs, nil))
		if err != nil || value != nil {
			t.Errorf("%s=%#v (%T), err=%v; want nil", src, value, value, err)
		}
	}
	value, err := expr.MustParse("concat(outputs.absent.items, outputs.r1.present.items)").Eval(e.exprContext(rs, nil))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := value.([]any)
	if !ok || len(got) != 1 || got[0] != "ok" {
		t.Fatalf("collector=%#v", value)
	}
}

func TestGroupOutputDoesNotFallBackToShorterNode(t *testing.T) {
	e := New(&ir.Workflow{Nodes: map[string]ir.Node{
		"r1":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "r1"}},
		"r1.gate": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "r1.gate"}},
	}}, tmpStore(t), newStubExecutor())
	rs := e.newRunState("absent-dotted", nil)
	rs.outputs["r1"] = map[string]any{"gate": map[string]any{"value": "wrong node"}, "other": "legitimate"}
	check := func(path []string, want any) {
		t.Helper()
		got := e.exprContext(rs, nil).Outputs(path)
		if got != want {
			t.Errorf("expression %v=%v, want %v", path, got, want)
		}
		got = e.resolveRef(&ir.Ref{Kind: ir.RefOutputs, Path: path}, rs.scope())
		if got != want {
			t.Errorf("template %v=%v, want %v", path, got, want)
		}
	}
	check([]string{"r1", "gate", "value"}, nil)
	check([]string{"r1", "gate"}, nil)
	check([]string{"r1", "other"}, "legitimate")
	rs.outputs["r1.gate"] = map[string]any{"value": "correct node"}
	check([]string{"r1", "gate", "value"}, "correct node")
}

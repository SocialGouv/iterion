package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestLiteralTemplateMappingAndFailNode(t *testing.T) {
	const raw = `{{"{{"}}vars.missing}} / {{vars.x}} / {{"{{"}}`
	const want = `{{vars.missing}} / {{vars.y}} / {{`
	wf := budgetedWorkflow()
	e := New(wf, tmpStore(t), newStubExecutor())
	rs := e.newRunState("literal-map", nil)
	rs.vars = map[string]any{"x": "{{vars.y}}", "y": "CASCADE"}
	if got := e.resolveMapping(mapping(t, raw), rs.scope()); got != want {
		t.Fatalf("mapping=%q", got)
	}
	if got := e.resolveMapping(mapping(t, `{{"{{"}}`), rs.scope()); got != "{{" {
		t.Fatalf("whole mapping=%q", got)
	}
	out := e.failOutcome(rs, &ir.FailNode{BaseNode: ir.BaseNode{ID: "f"}, Message: mapping(t, raw)})
	if out.reason != want {
		t.Fatalf("fail reason=%q", out.reason)
	}
}

func TestLiteralTemplateCompiledMappingReachesNextNode(t *testing.T) {
	const src = `dsl: 2
schema out:
  text: string
agent start:
  model: "test-model"
  output: out
agent finish:
  model: "test-model"
  input: out
  output: out
workflow w:
  worktree: none
  entry: start
  start -> finish with { text: "{{\"{{\"}}vars.missing}} / {{outputs.start.text}}" }
  finish -> done
`
	cr := compileBot(t, src)
	if cr.HasErrors() {
		t.Fatal(cr.Diagnostics)
	}
	exec := newStubExecutor()
	exec.on("start", func(map[string]any) (map[string]any, error) {
		return map[string]any{"text": "{{outputs.start.text}}"}, nil
	})
	called := false
	exec.on("finish", func(input map[string]any) (map[string]any, error) {
		called = true
		if input["text"] != `{{vars.missing}} / {{outputs.start.text}}` {
			t.Errorf("input=%v", input)
		}
		return input, nil
	})
	if err := New(cr.Workflow, tmpStore(t), exec).Run(context.Background(), "literal-edge", nil); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("mapping was not consumed")
	}
}

func TestLiteralTemplateForeachRefIsAStringNotAnArray(t *testing.T) {
	const raw = `{{"{{"}}`
	refs, err := ir.ParseRefs(raw)
	if err != nil {
		t.Fatal(err)
	}
	wf := foreachWorkflow()
	wf.Foreaches["scan"].CollectionRaw = raw
	wf.Foreaches["scan"].CollectionRefs = refs
	e := New(wf, tmpStore(t), newStubExecutor())
	rs := e.newRunState("literal-foreach", nil)
	if value := e.resolveRef(refs[0], rs.scope()); value != "{{" {
		t.Fatalf("collection reference=%v", value)
	}
	if got := e.resolveForeachCollection(wf.Foreaches["scan"], rs.scope()); len(got) != 0 {
		t.Fatalf("non-array literal produced items=%v", got)
	}
}

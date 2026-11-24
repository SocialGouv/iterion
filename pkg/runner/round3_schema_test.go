package runner

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

type r3SchemaExecutor struct{ calls int }

func (e *r3SchemaExecutor) Execute(context.Context, ir.Node, map[string]any) (map[string]any, error) {
	e.calls++
	return map[string]any{"ok": "invalid"}, nil
}

func TestR3StrictToolSchemaAcksWithoutRepeatingEffects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	for _, kind := range []string{"tool", "agent"} {
		t.Run(kind, func(t *testing.T) {
			var node ir.Node = &ir.ToolNode{BaseNode: ir.BaseNode{ID: "work"}, SchemaFields: ir.SchemaFields{OutputSchema: "result"}}
			if kind == "agent" {
				node = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}, SchemaFields: ir.SchemaFields{OutputSchema: "result"}}
			}
			wf := &ir.Workflow{Name: "r3", Entry: "work", Nodes: map[string]ir.Node{"work": node}, Schemas: map[string]*ir.Schema{"result": {Name: "result", Fields: []*ir.SchemaField{{Name: "ok", Type: ir.FieldTypeBool}}}}}
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			exec := &r3SchemaExecutor{}
			err = runtime.New(wf, st, exec, runtime.WithWorkDir(t.TempDir()), runtime.WithOutputValidation(true)).Run(context.Background(), "r3", nil)
			outcome := classifyExecResult(err, "r3")
			if exec.calls != 1 || err == nil {
				t.Fatalf("calls=%d err=%v", exec.calls, err)
			}
			if kind == "tool" && outcome.op != "ack-deterministic-failure" {
				t.Fatalf("tool would replay effects: %+v err=%v", outcome, err)
			}
			if kind == "agent" && !isNakAction(outcome.action) {
				t.Fatalf("LLM cannot reexecute: %+v", outcome)
			}
		})
	}
}

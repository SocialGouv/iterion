package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Execute the source-level macro through the real compiler and engine. Both
// compute expressions and edge templates must resolve each instance's own data.
func TestGroupLocalOutputsExecuteIndependently(t *testing.T) {
	src := `schema out:
  value: string
group g(label):
  compute seed:
    output: out
    expr:
      value: "'{{params.label}}'"
  compute calc:
    output: out
    expr:
      value: "outputs.seed.value"
  agent report:
    model: "test-model"
    input: out
    output: out
    system: "{{params.label}}: {{outputs.calc.value}}"
  seed -> calc
  calc -> report when "outputs.calc.value != ''" with {value: "{{outputs.calc.value}}"}
  calc -> fail else
use g as r1 with {label: "A"}
use g as r2 with {label: "B"}
workflow w:
  worktree: none
  entry: r1.seed
  r1.report -> r2.seed
  r2.report -> done
`
	pr := parser.Parse("groups.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatal(d.Error())
		}
	}
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatal(d.Error())
		}
	}
	exec := newStubExecutor()
	for node, want := range map[string]string{"r1.report": "A", "r2.report": "B"} {
		exec.on(node, func(input map[string]any) (map[string]any, error) {
			if input["value"] != want {
				t.Errorf("%s input=%v, want %s", node, input, want)
			}
			return input, nil
		})
	}
	s := tmpStore(t)
	if err := New(cr.Workflow, s, exec).Run(context.Background(), "groups", nil); err != nil {
		t.Fatal(err)
	}
	r, err := s.LoadRun(context.Background(), "groups")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status=%s, error=%s", r.Status, r.Error)
	}
	if r.Checkpoint == nil {
		t.Fatal("missing persisted checkpoint")
	}
	for node, want := range map[string]string{"r1.calc": "A", "r2.calc": "B", "r1.report": "A", "r2.report": "B"} {
		if got := r.Checkpoint.Outputs[node]["value"]; got != want {
			t.Errorf("%s persisted %v, want %s", node, got, want)
		}
	}
}

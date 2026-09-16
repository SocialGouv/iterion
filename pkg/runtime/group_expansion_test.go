package runtime

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"strings"
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
  tool check:
    command: "test {{outputs.calc.value}} = {{params.label}}"
    postcondition: "test {{outputs.calc.value}} = {{params.label}}"
    policy: required
  report -> check
  seed -> calc
  calc -> report when "outputs.calc.value != ''" with {value: "{{outputs.calc.value}}"}
  calc -> fail else
use g as r1 with {label: "A"}
use g as r2 with {label: "B"}
workflow w:
  worktree: none
  entry: r1.seed
  r1.check -> r2.seed
  r2.check -> done
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
	b := &groupPromptBackend{t: t}
	reg := delegate.NewRegistry()
	reg.Register("group_test", b)
	for _, node := range cr.Workflow.Nodes {
		if a, ok := node.(*ir.AgentNode); ok {
			a.Backend = "group_test"
		}
	}
	exec := model.NewClawExecutor(model.NewRegistry(), cr.Workflow,
		model.WithBackendRegistry(reg), model.WithWorkDir(t.TempDir()),
		model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	s := tmpStore(t)
	if err := New(cr.Workflow, s, exec).Run(context.Background(), "groups", nil); err != nil {
		t.Fatal(err)
	}
	if b.calls != 2 {
		t.Fatalf("agent calls=%d, want 2", b.calls)
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

// The real executor must render the specialized prompt before delegation.
// Checking only edge inputs misses a distinct resolver used by tools/prompts.
type groupPromptBackend struct {
	t     *testing.T
	calls int
}

func (b *groupPromptBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.calls++
	want := map[string]string{"r1.report": "A", "r2.report": "B"}[task.NodeID]
	if want == "" || !strings.Contains(task.SystemPrompt, want+": "+want) || strings.Contains(task.SystemPrompt, "{{") {
		b.t.Errorf("%s system prompt = %q", task.NodeID, task.SystemPrompt)
	}
	return delegate.Result{Output: map[string]any{"value": want}, BackendName: "group_test"}, nil
}

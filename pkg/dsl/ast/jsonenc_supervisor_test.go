package ast_test

import (
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The AST JSON codec is the wire format of the cloud queue: a launch
// serializes the parsed File with MarshalFile and the runner pod rebuilds
// it with UnmarshalFile before ir.Compile. A declaration the codec drops
// therefore exists locally and silently disappears on every cloud run —
// which is exactly how supervisor declarations shipped: found live on a
// prod runner pod (run 01a03d70, no spawn and no skip log).
func TestSupervisorSurvivesJSONRoundTrip(t *testing.T) {
	f := &ast.File{
		Supervisors: []*ast.SupervisorDecl{{
			Name:     "persy",
			Watches:  []string{"campaign"},
			Model:    "anthropic/claude-opus-5",
			System:   "persy_policy",
			Cooldown: "5m",
			MaxEvals: 10,
			Monitors: []string{"event_type=budget_warning", "event_type=assistant_text,text_contains=impossible"},
		}},
	}

	raw, err := ast.MarshalFile(f)
	if err != nil {
		t.Fatalf("MarshalFile: %v", err)
	}
	back, err := ast.UnmarshalFile(raw)
	if err != nil {
		t.Fatalf("UnmarshalFile: %v", err)
	}
	if len(back.Supervisors) != 1 {
		t.Fatalf("supervisors dropped by the JSON round-trip: got %d, want 1", len(back.Supervisors))
	}
	got, want := back.Supervisors[0], f.Supervisors[0]
	// Span is positional noise the codec does not carry; compare the rest.
	got.Span = want.Span
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("supervisor mangled by round-trip:\n got %+v\nwant %+v", got, want)
	}
}

// The composition the runner actually executes: parse → MarshalFile →
// UnmarshalFile → ir.Compile must yield a workflow whose Supervisors are
// intact, or the pod spawns nothing without even a skip log.
func TestSupervisorSurvivesQueueCompilePath(t *testing.T) {
	src := `
prompt policy:
  body: "Keep the agent honest."

prompt sys:
  body: "Do the work."

schema out:
  summary: string

supervisor coach:
  watches: [work]
  system: policy
  cooldown: "2m"
  max_evals: 5
  monitors: ["event_type=budget_warning"]

agent work:
  system: sys
  output: out

workflow w:
  entry: work
  work -> done
`
	res := parser.Parse("test.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("parse: %+v", res.Diagnostics)
	}
	raw, err := ast.MarshalFile(res.File)
	if err != nil {
		t.Fatalf("MarshalFile: %v", err)
	}
	back, err := ast.UnmarshalFile(raw)
	if err != nil {
		t.Fatalf("UnmarshalFile: %v", err)
	}
	cr := ir.Compile(back)
	if cr.HasErrors() {
		t.Fatalf("compile: %v", cr.Diagnostics)
	}
	if len(cr.Workflow.Supervisors) != 1 {
		t.Fatalf("compiled workflow lost its supervisor after the queue round-trip: got %d, want 1", len(cr.Workflow.Supervisors))
	}
	sup := cr.Workflow.Supervisors[0]
	if sup.Name != "coach" || len(sup.Monitors) != 1 {
		t.Fatalf("supervisor fields wrong after round-trip: %+v", sup)
	}
}

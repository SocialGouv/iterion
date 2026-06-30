package ir

import "testing"

const subbotSrc = `
schema verdict:
  validated: bool

tool plan:
  command: "true"
  output: verdict

subbot run_ticket:
  source: "child.bot"
  with { issue: "{{outputs.plan.validated}}" }
  output: verdict

workflow w:
  entry: plan

  plan -> run_ticket
  run_ticket -> done when validated
  run_ticket -> fail
`

// TestCompileSubbot checks a subbot node compiles into a SubbotNode carrying
// its source, with-mappings and output schema.
func TestCompileSubbot(t *testing.T) {
	w := mustCompile(t, subbotSrc)
	n, ok := w.Nodes["run_ticket"].(*SubbotNode)
	if !ok {
		t.Fatalf("run_ticket is not a SubbotNode: %T", w.Nodes["run_ticket"])
	}
	if n.NodeKind() != NodeSubbot {
		t.Fatalf("NodeKind = %v, want subbot", n.NodeKind())
	}
	if n.Source != "child.bot" || n.OutputSchema != "verdict" {
		t.Fatalf("source/output = %q/%q", n.Source, n.OutputSchema)
	}
	if len(n.With) != 1 || n.With[0].Key != "issue" {
		t.Fatalf("with mappings = %v", n.With)
	}
}

// TestSubbotNoSource asserts C119 fires for a subbot without a source.
func TestSubbotNoSource(t *testing.T) {
	src := `
schema empty:
  ok: bool

subbot run_ticket:
  output: empty

tool a:
  command: "true"
  output: empty

workflow w:
  entry: a
  a -> run_ticket
  run_ticket -> done
`
	if !hasDiag(compileFile(t, src).Diagnostics, DiagSubbotNoSource) {
		t.Fatal("expected C119 (DiagSubbotNoSource)")
	}
}

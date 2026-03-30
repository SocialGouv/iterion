package ir_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/parser"
)

// compileTestWorkflow parses and compiles an inline workflow DSL string.
func compileTestWorkflow(t *testing.T, src string) *ir.Workflow {
	t.Helper()
	pr := parser.Parse("test.iter", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s", d.Error())
		}
	}
	cr := ir.Compile(pr.File)
	if cr.HasErrors() {
		for _, d := range cr.Diagnostics {
			if d.Severity == ir.SeverityError {
				t.Fatalf("compile error: %s", d.Error())
			}
		}
	}
	return cr.Workflow
}

// ---------------------------------------------------------------------------
// Minimal workflow
// ---------------------------------------------------------------------------

const minimalDSL = `
prompt sys:
  You are a reviewer.

prompt usr:
  Review: {{input.description}}

schema review_input:
  description: string

schema review_output:
  approved: bool
  summary: string

agent reviewer:
  model: "test-model"
  input: review_input
  output: review_output
  system: sys
  user: usr
  session: fresh

workflow test_workflow:
  entry: reviewer

  reviewer -> done when approved
  reviewer -> fail when not approved
`

func TestMermaid_Compact_MinimalWorkflow(t *testing.T) {
	wf := compileTestWorkflow(t, minimalDSL)
	out := wf.ToMermaid(ir.MermaidCompact)

	// Must start with flowchart directive.
	if !strings.HasPrefix(out, "flowchart TD\n") {
		t.Errorf("expected flowchart TD header, got:\n%s", out)
	}

	// Must contain all nodes.
	for _, id := range []string{"reviewer", "done", "fail"} {
		if !strings.Contains(out, id) {
			t.Errorf("expected node %q in output, got:\n%s", id, out)
		}
	}

	// Must contain edge conditions.
	if !strings.Contains(out, "approved") {
		t.Errorf("expected condition 'approved' in output, got:\n%s", out)
	}
	if !strings.Contains(out, "NOT approved") {
		t.Errorf("expected 'NOT approved' in output, got:\n%s", out)
	}

	// Must contain style classes.
	if !strings.Contains(out, "classDef agent") {
		t.Errorf("expected classDef in output, got:\n%s", out)
	}
}

func TestMermaid_Detailed_MinimalWorkflow(t *testing.T) {
	wf := compileTestWorkflow(t, minimalDSL)
	out := wf.ToMermaid(ir.MermaidDetailed)

	// Detailed view should contain model info.
	if !strings.Contains(out, "model: test-model") {
		t.Errorf("expected model info in detailed view, got:\n%s", out)
	}

	// Should contain schema references.
	if !strings.Contains(out, "in: review_input") {
		t.Errorf("expected input schema in detailed view, got:\n%s", out)
	}
	if !strings.Contains(out, "out: review_output") {
		t.Errorf("expected output schema in detailed view, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Workflow with human, join, router
// ---------------------------------------------------------------------------

const complexDSL = `
prompt sys:
  You are a reviewer.

prompt usr:
  Review: {{input.description}}

prompt human_instr:
  Please provide feedback.

schema input_s:
  description: string

schema output_s:
  approved: bool

schema human_out:
  feedback: string

agent reviewer_a:
  model: "model-a"
  input: input_s
  output: output_s
  system: sys
  user: usr
  session: fresh

agent reviewer_b:
  model: "model-b"
  input: input_s
  output: output_s
  system: sys
  user: usr
  session: fresh

router fanout:
  mode: fan_out_all

join merge:
  strategy: wait_all
  require: [reviewer_a, reviewer_b]
  output: output_s

human checkpoint:
  mode: pause_until_answers
  input: output_s
  output: human_out
  instructions: human_instr
  min_answers: 1

workflow complex_workflow:
  entry: fanout

  fanout -> reviewer_a
  fanout -> reviewer_b
  reviewer_a -> merge
  reviewer_b -> merge
  merge -> checkpoint when not approved
  merge -> done when approved
  checkpoint -> done
`

func TestMermaid_Compact_ComplexWorkflow(t *testing.T) {
	wf := compileTestWorkflow(t, complexDSL)
	out := wf.ToMermaid(ir.MermaidCompact)

	// Check all node types are present.
	expected := []string{"fanout", "reviewer_a", "reviewer_b", "merge", "checkpoint", "done"}
	for _, id := range expected {
		if !strings.Contains(out, id) {
			t.Errorf("expected node %q in output, got:\n%s", id, out)
		}
	}

	// Router should use diamond shape {}.
	if !strings.Contains(out, "{") {
		t.Errorf("expected diamond shape for router, got:\n%s", out)
	}

	// Join should use double bracket [[]].
	if !strings.Contains(out, "[[") {
		t.Errorf("expected double bracket for join, got:\n%s", out)
	}

	// Human should use asymmetric shape >.
	if !strings.Contains(out, ">") {
		t.Errorf("expected asymmetric shape for human, got:\n%s", out)
	}
}

func TestMermaid_Detailed_ComplexWorkflow(t *testing.T) {
	wf := compileTestWorkflow(t, complexDSL)
	out := wf.ToMermaid(ir.MermaidDetailed)

	// Router mode should appear.
	if !strings.Contains(out, "mode: fan_out_all") {
		t.Errorf("expected router mode in detailed view, got:\n%s", out)
	}

	// Join strategy should appear.
	if !strings.Contains(out, "strategy: wait_all") {
		t.Errorf("expected join strategy in detailed view, got:\n%s", out)
	}

	// Human mode should appear.
	if !strings.Contains(out, "mode: pause_until_answers") {
		t.Errorf("expected human mode in detailed view, got:\n%s", out)
	}

	// Require list should appear.
	if !strings.Contains(out, "require: reviewer_a, reviewer_b") {
		t.Errorf("expected require list in detailed view, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Workflow with loops
// ---------------------------------------------------------------------------

const loopDSL = `
prompt sys:
  You are a refiner.

prompt usr:
  Refine: {{input.text}}

schema refine_input:
  text: string

schema refine_output:
  approved: bool
  refined: string

agent refiner:
  model: "test-model"
  input: refine_input
  output: refine_output
  system: sys
  user: usr
  session: fresh

agent checker:
  model: "test-model"
  input: refine_output
  output: refine_output
  system: sys
  user: usr
  session: fresh

workflow loop_workflow:
  entry: refiner

  refiner -> checker
  checker -> done when approved
  checker -> refiner when not approved as refine_loop(3)
`

func TestMermaid_Compact_LoopWorkflow(t *testing.T) {
	wf := compileTestWorkflow(t, loopDSL)
	out := wf.ToMermaid(ir.MermaidCompact)

	// Loop annotation should appear on the edge.
	if !strings.Contains(out, "loop:refine_loop(3)") {
		t.Errorf("expected loop annotation on edge, got:\n%s", out)
	}

	// Both condition and loop should be present.
	if !strings.Contains(out, "NOT approved") {
		t.Errorf("expected condition 'NOT approved' in output, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Golden output test — compact view of minimal workflow
// ---------------------------------------------------------------------------

func TestMermaid_GoldenCompact(t *testing.T) {
	wf := compileTestWorkflow(t, minimalDSL)
	got := wf.ToMermaid(ir.MermaidCompact)

	// Verify structural properties rather than exact string to avoid
	// brittleness, but check key invariants.
	lines := strings.Split(strings.TrimSpace(got), "\n")

	if lines[0] != "flowchart TD" {
		t.Errorf("first line should be 'flowchart TD', got %q", lines[0])
	}

	// Count node declarations (non-empty lines with node shapes).
	var nodeLines, edgeLines, classLines int
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "flowchart TD" {
			continue
		}
		if strings.HasPrefix(trimmed, "classDef") || strings.HasPrefix(trimmed, "class ") {
			classLines++
		} else if strings.Contains(trimmed, "-->") {
			edgeLines++
		} else if len(trimmed) > 0 {
			nodeLines++
		}
	}

	if nodeLines != 3 { // reviewer, done, fail
		t.Errorf("expected 3 node declarations, got %d", nodeLines)
	}
	if edgeLines != 2 { // reviewer->done, reviewer->fail
		t.Errorf("expected 2 edge declarations, got %d", edgeLines)
	}
	if classLines == 0 {
		t.Error("expected style class definitions")
	}
}

// ---------------------------------------------------------------------------
// Edge: empty workflow (just entry → done)
// ---------------------------------------------------------------------------

const trivialDSL = `
prompt sys:
  Hello.

prompt usr:
  Do: {{input.task}}

schema task_input:
  task: string

agent worker:
  model: "m"
  input: task_input
  system: sys
  user: usr
  session: fresh

workflow trivial:
  entry: worker

  worker -> done
`

func TestMermaid_TrivialWorkflow(t *testing.T) {
	wf := compileTestWorkflow(t, trivialDSL)
	out := wf.ToMermaid(ir.MermaidCompact)

	// Should have worker → done edge with no label.
	if !strings.Contains(out, "worker --> done") {
		t.Errorf("expected unlabeled edge worker --> done, got:\n%s", out)
	}
}

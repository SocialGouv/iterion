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
// Workflow with human, convergence, router
// ---------------------------------------------------------------------------

const complexDSL = `
prompt sys:
  You are a reviewer.

prompt usr:
  Review: {{input.description}}

prompt merge_usr:
  Merge reviews.

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

agent merge:
  model: "model-a"
  input: output_s
  output: output_s
  system: sys
  user: merge_usr
  await: wait_all

human checkpoint:
  interaction: human
  input: output_s
  output: human_out
  instructions: human_instr
  min_answers: 1

workflow complex_workflow:
  entry: fanout

  fanout -> reviewer_a
  fanout -> reviewer_b
  reviewer_a -> merge with { review_a: "{{outputs.reviewer_a}}" }
  reviewer_b -> merge with { review_b: "{{outputs.reviewer_b}}" }
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

	// Convergence node (merge) should be present.
	if !strings.Contains(out, "merge") {
		t.Errorf("expected merge node in output, got:\n%s", out)
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

	// Await strategy should appear on merge node.
	if !strings.Contains(out, "await: wait_all") {
		t.Errorf("expected await strategy in detailed view, got:\n%s", out)
	}

	// Human mode should appear.
	if !strings.Contains(out, "interaction: human") {
		t.Errorf("expected human mode in detailed view, got:\n%s", out)
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

prompt checker_usr:
  Check the refinement.

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
  user: checker_usr
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

// ---------------------------------------------------------------------------
// Full view — minimal workflow
// ---------------------------------------------------------------------------

func TestMermaid_Full_MinimalWorkflow(t *testing.T) {
	wf := compileTestWorkflow(t, minimalDSL)
	out := wf.ToMermaid(ir.MermaidFull)

	// Must contain model info (like detailed).
	if !strings.Contains(out, "model: test-model") {
		t.Errorf("expected model info in full view, got:\n%s", out)
	}

	// Must contain expanded schema fields.
	if !strings.Contains(out, "description: string") {
		t.Errorf("expected expanded input schema fields, got:\n%s", out)
	}
	if !strings.Contains(out, "approved: bool") {
		t.Errorf("expected expanded output schema fields, got:\n%s", out)
	}

	// Must show session: fresh explicitly.
	if !strings.Contains(out, "session: fresh") {
		t.Errorf("expected session: fresh in full view, got:\n%s", out)
	}

	// Must show prompt references.
	if !strings.Contains(out, "system: sys") {
		t.Errorf("expected system prompt ref in full view, got:\n%s", out)
	}
	if !strings.Contains(out, "user: usr") {
		t.Errorf("expected user prompt ref in full view, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Full view — complex workflow
// ---------------------------------------------------------------------------

func TestMermaid_Full_ComplexWorkflow(t *testing.T) {
	wf := compileTestWorkflow(t, complexDSL)
	out := wf.ToMermaid(ir.MermaidFull)

	// Router mode should appear.
	if !strings.Contains(out, "mode: fan_out_all") {
		t.Errorf("expected router mode in full view, got:\n%s", out)
	}

	// Await strategy should appear on convergence node.
	if !strings.Contains(out, "await: wait_all") {
		t.Errorf("expected await strategy in full view, got:\n%s", out)
	}

	// Human instructions prompt should appear.
	if !strings.Contains(out, "instructions: human_instr") {
		t.Errorf("expected human instructions in full view, got:\n%s", out)
	}

	// Human mode should appear.
	if !strings.Contains(out, "interaction: human") {
		t.Errorf("expected human mode in full view, got:\n%s", out)
	}

	// Expanded schema fields for human output.
	if !strings.Contains(out, "feedback: string") {
		t.Errorf("expected expanded human output schema, got:\n%s", out)
	}

	// Output schema should appear on merge node.
	if !strings.Contains(out, "out: output_s") {
		t.Errorf("expected output schema in full view, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Full view — workflow metadata (vars, budget, loops)
// ---------------------------------------------------------------------------

const fullMetaDSL = `
prompt sys:
  Hello.

prompt usr:
  Do: {{input.task}}

schema task_input:
  task: string

schema task_output:
  ok: bool

agent worker:
  model: "m"
  input: task_input
  output: task_output
  system: sys
  user: usr
  session: fresh

workflow meta_wf:
  vars:
    name: string = "default"
    count: int = 5

  entry: worker

  budget:
    max_parallel_branches: 2
    max_duration: "30m"
    max_cost_usd: 10.50
    max_tokens: 100000

  worker -> done when ok
  worker -> worker when not ok as retry(3)
`

func TestMermaid_Full_WorkflowMetadata(t *testing.T) {
	wf := compileTestWorkflow(t, fullMetaDSL)
	out := wf.ToMermaid(ir.MermaidFull)

	// Must contain subgraph with workflow name.
	if !strings.Contains(out, "subgraph") {
		t.Errorf("expected subgraph in full view, got:\n%s", out)
	}
	if !strings.Contains(out, "meta_wf") {
		t.Errorf("expected workflow name in subgraph, got:\n%s", out)
	}

	// Must contain variables.
	if !strings.Contains(out, "Variables") {
		t.Errorf("expected Variables header in metadata, got:\n%s", out)
	}
	if !strings.Contains(out, "name: string") {
		t.Errorf("expected var 'name' in metadata, got:\n%s", out)
	}
	if !strings.Contains(out, "count: int") {
		t.Errorf("expected var 'count' in metadata, got:\n%s", out)
	}

	// Must contain budget.
	if !strings.Contains(out, "Budget") {
		t.Errorf("expected Budget header in metadata, got:\n%s", out)
	}
	if !strings.Contains(out, "max_parallel: 2") {
		t.Errorf("expected max_parallel in budget, got:\n%s", out)
	}
	if !strings.Contains(out, "max_duration: 30m") {
		t.Errorf("expected max_duration in budget, got:\n%s", out)
	}
	if !strings.Contains(out, "$10.50") {
		t.Errorf("expected max_cost in budget, got:\n%s", out)
	}

	// Must contain loops.
	if !strings.Contains(out, "Loops") {
		t.Errorf("expected Loops header in metadata, got:\n%s", out)
	}
	if !strings.Contains(out, "retry: max 3") {
		t.Errorf("expected loop 'retry' in metadata, got:\n%s", out)
	}

	// Must contain entry point info.
	if !strings.Contains(out, "entry: worker") {
		t.Errorf("expected entry point in metadata, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Full view — edge with data mappings
// ---------------------------------------------------------------------------

const fullEdgeDSL = `
prompt sys:
  Hello.

prompt usr:
  Do: {{input.text}}

schema s_in:
  text: string

schema s_out:
  result: string

agent step1:
  model: "m"
  input: s_in
  output: s_out
  system: sys
  user: usr
  session: fresh

agent step2:
  model: "m"
  input: s_in
  output: s_out
  system: sys
  user: usr
  session: fresh

workflow edge_wf:
  entry: step1

  step1 -> step2 with {
    text: "{{outputs.step1.result}}"
  }
  step2 -> done
`

func TestMermaid_Full_EdgeMappings(t *testing.T) {
	wf := compileTestWorkflow(t, fullEdgeDSL)
	out := wf.ToMermaid(ir.MermaidFull)

	// Data mappings should appear on edges (like detailed view).
	if !strings.Contains(out, "with:") {
		t.Errorf("expected data mappings on edge in full view, got:\n%s", out)
	}
	if !strings.Contains(out, "text=") {
		t.Errorf("expected mapping key in full view, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Full view — schema field cap (+N more)
// ---------------------------------------------------------------------------

const manyFieldsDSL = `
prompt sys:
  Hello.

prompt usr:
  Do: {{input.f1}}

schema big_input:
  f1: string
  f2: string
  f3: string
  f4: string
  f5: string
  f6: string

agent worker:
  model: "m"
  input: big_input
  system: sys
  user: usr
  session: fresh

workflow cap_wf:
  entry: worker

  worker -> done
`

func TestMermaid_Full_SchemaFieldCap(t *testing.T) {
	wf := compileTestWorkflow(t, manyFieldsDSL)
	out := wf.ToMermaid(ir.MermaidFull)

	// Should show first 4 fields and +2 more.
	if !strings.Contains(out, "+2 more") {
		t.Errorf("expected '+2 more' for capped schema, got:\n%s", out)
	}
	// Should contain f1 through f4.
	for _, f := range []string{"f1: string", "f2: string", "f3: string", "f4: string"} {
		if !strings.Contains(out, f) {
			t.Errorf("expected field %q in output, got:\n%s", f, out)
		}
	}
}

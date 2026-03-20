package ir

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iterion-ai/iterion/ast"
	"github.com/iterion-ai/iterion/parser"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func parseFile(t *testing.T, src string) *ast.File {
	t.Helper()
	res := parser.Parse("test.iter", src)
	for _, d := range res.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s", d.Error())
		}
	}
	return res.File
}

func compileFile(t *testing.T, src string) *CompileResult {
	t.Helper()
	return Compile(parseFile(t, src))
}

func mustCompile(t *testing.T, src string) *Workflow {
	t.Helper()
	r := compileFile(t, src)
	for _, d := range r.Diagnostics {
		if d.Severity == SeverityError {
			t.Fatalf("compile error: %s", d.Error())
		}
	}
	if r.Workflow == nil {
		t.Fatal("expected non-nil workflow")
	}
	return r.Workflow
}

// ---------------------------------------------------------------------------
// Unit tests — minimal workflow
// ---------------------------------------------------------------------------

const minimalSrc = `
schema empty:
  ok: bool

prompt sys:
  You are a minimal agent.

prompt usr:
  Do something.

agent start:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr

workflow minimal:
  entry: start
  start -> done
`

func TestCompileMinimalWorkflow(t *testing.T) {
	w := mustCompile(t, minimalSrc)

	if w.Name != "minimal" {
		t.Errorf("expected name 'minimal', got %q", w.Name)
	}
	if w.Entry != "start" {
		t.Errorf("expected entry 'start', got %q", w.Entry)
	}

	// 3 nodes: start + done + fail
	if len(w.Nodes) != 3 {
		t.Errorf("expected 3 nodes, got %d", len(w.Nodes))
	}
	if w.Nodes["start"].Kind != NodeAgent {
		t.Errorf("start should be agent")
	}
	if w.Nodes["done"].Kind != NodeDone {
		t.Errorf("done should be NodeDone")
	}
	if w.Nodes["fail"].Kind != NodeFail {
		t.Errorf("fail should be NodeFail")
	}

	// 1 edge
	if len(w.Edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(w.Edges))
	}
	if w.Edges[0].From != "start" || w.Edges[0].To != "done" {
		t.Errorf("expected edge start->done, got %s->%s", w.Edges[0].From, w.Edges[0].To)
	}

	// Schema and prompt resolved
	if _, ok := w.Schemas["empty"]; !ok {
		t.Error("schema 'empty' not found")
	}
	if _, ok := w.Prompts["sys"]; !ok {
		t.Error("prompt 'sys' not found")
	}
}

// ---------------------------------------------------------------------------
// Nodes: all kinds
// ---------------------------------------------------------------------------

const allNodesSrc = `
schema in_s:
  x: string

schema out_s:
  y: bool

schema join_out:
  merged: json

schema human_out:
  answered: bool

schema tool_out:
  result: string

prompt sys:
  System prompt.

prompt usr:
  User prompt.

prompt human_instr:
  Answer the questions.

agent a1:
  model: "claude"
  input: in_s
  output: out_s
  system: sys
  user: usr
  session: inherit
  tools: [read_file, write_file]
  tool_max_steps: 5

judge j1:
  model: "gpt"
  input: in_s
  output: out_s
  system: sys
  user: usr

router r1:
  mode: fan_out_all

join jn1:
  strategy: wait_all
  require: [a1, j1]
  output: join_out

human h1:
  input: in_s
  output: human_out
  instructions: human_instr
  mode: pause_until_answers
  min_answers: 2

tool t1:
  command: "go test ./..."
  output: tool_out

workflow all_nodes:
  entry: a1
  a1 -> done
`

func TestCompileAllNodeKinds(t *testing.T) {
	w := mustCompile(t, allNodesSrc)

	tests := []struct {
		id   string
		kind NodeKind
	}{
		{"a1", NodeAgent},
		{"j1", NodeJudge},
		{"r1", NodeRouter},
		{"jn1", NodeJoin},
		{"h1", NodeHuman},
		{"t1", NodeTool},
		{"done", NodeDone},
		{"fail", NodeFail},
	}
	for _, tt := range tests {
		n, ok := w.Nodes[tt.id]
		if !ok {
			t.Errorf("node %q not found", tt.id)
			continue
		}
		if n.Kind != tt.kind {
			t.Errorf("node %q: expected kind %v, got %v", tt.id, tt.kind, n.Kind)
		}
	}

	// Agent details
	a1 := w.Nodes["a1"]
	if a1.Model != "claude" {
		t.Errorf("a1 model: expected 'claude', got %q", a1.Model)
	}
	if a1.Session != SessionInherit {
		t.Errorf("a1 session: expected inherit, got %v", a1.Session)
	}
	if len(a1.Tools) != 2 {
		t.Errorf("a1 tools: expected 2, got %d", len(a1.Tools))
	}
	if a1.ToolMaxSteps != 5 {
		t.Errorf("a1 tool_max_steps: expected 5, got %d", a1.ToolMaxSteps)
	}

	// Join details
	jn1 := w.Nodes["jn1"]
	if jn1.JoinStrategy != JoinWaitAll {
		t.Errorf("jn1 strategy: expected wait_all, got %v", jn1.JoinStrategy)
	}
	if len(jn1.Require) != 2 {
		t.Errorf("jn1 require: expected 2, got %d", len(jn1.Require))
	}

	// Human details
	h1 := w.Nodes["h1"]
	if h1.MinAnswers != 2 {
		t.Errorf("h1 min_answers: expected 2, got %d", h1.MinAnswers)
	}
	if h1.Instructions != "human_instr" {
		t.Errorf("h1 instructions: expected 'human_instr', got %q", h1.Instructions)
	}

	// Router details
	r1 := w.Nodes["r1"]
	if r1.RouterMode != RouterFanOutAll {
		t.Errorf("r1 mode: expected fan_out_all, got %v", r1.RouterMode)
	}

	// Tool details
	t1 := w.Nodes["t1"]
	if t1.Command != "go test ./..." {
		t.Errorf("t1 command: expected 'go test ./...', got %q", t1.Command)
	}
}

// ---------------------------------------------------------------------------
// Edges: conditions, loops, with mappings
// ---------------------------------------------------------------------------

const edgesSrc = `
schema s:
  approved: bool

prompt sys:
  System.

prompt usr:
  User.

judge check:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent refine:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow edge_test:
  entry: check
  check -> done when approved
  check -> refine when not_approved as refine_loop(5) with {
    plan: "{{outputs.check}}",
    context: "{{vars.review_rules}}"
  }
  refine -> check
`

func TestCompileEdges(t *testing.T) {
	w := mustCompile(t, edgesSrc)

	if len(w.Edges) != 3 {
		t.Fatalf("expected 3 edges, got %d", len(w.Edges))
	}

	// Edge 0: check -> done when approved
	e0 := w.Edges[0]
	if e0.From != "check" || e0.To != "done" {
		t.Errorf("edge 0: expected check->done, got %s->%s", e0.From, e0.To)
	}
	if e0.Condition != "approved" || e0.Negated {
		t.Errorf("edge 0: expected condition=approved negated=false, got %q/%v", e0.Condition, e0.Negated)
	}

	// Edge 1: check -> refine when not_approved as refine_loop(5) with {...}
	e1 := w.Edges[1]
	// "not_approved" is a single identifier, not "not" + "approved"
	if e1.Condition != "not_approved" {
		t.Errorf("edge 1: expected condition=not_approved, got %q", e1.Condition)
	}
	if e1.Negated {
		t.Error("edge 1: expected negated=false (not_approved is a single identifier)")
	}
	if e1.LoopName != "refine_loop" {
		t.Errorf("edge 1: expected loop refine_loop, got %q", e1.LoopName)
	}
	if len(e1.With) != 2 {
		t.Fatalf("edge 1: expected 2 with mappings, got %d", len(e1.With))
	}
	// First mapping: plan -> {{outputs.check}}
	if e1.With[0].Key != "plan" {
		t.Errorf("with[0] key: expected 'plan', got %q", e1.With[0].Key)
	}
	if len(e1.With[0].Refs) != 1 || e1.With[0].Refs[0].Kind != RefOutputs {
		t.Errorf("with[0] ref: expected RefOutputs")
	}
	// Second mapping: context -> {{vars.review_rules}}
	if e1.With[1].Key != "context" {
		t.Errorf("with[1] key: expected 'context', got %q", e1.With[1].Key)
	}
	if len(e1.With[1].Refs) != 1 || e1.With[1].Refs[0].Kind != RefVars {
		t.Errorf("with[1] ref: expected RefVars")
	}

	// Loop definition
	loop, ok := w.Loops["refine_loop"]
	if !ok {
		t.Fatal("loop 'refine_loop' not found")
	}
	if loop.MaxIterations != 5 {
		t.Errorf("loop max_iterations: expected 5, got %d", loop.MaxIterations)
	}
}

// ---------------------------------------------------------------------------
// Vars: top-level + workflow-level merge
// ---------------------------------------------------------------------------

const varsSrc = `
vars:
  global_var: string = "default_global"

schema s:
  ok: bool

prompt sys:
  Sys.

prompt usr:
  Usr.

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow var_test:
  vars:
    local_var: int = 42
    global_var: string = "overridden"
  entry: a
  a -> done
`

func TestCompileVars(t *testing.T) {
	w := mustCompile(t, varsSrc)

	if len(w.Vars) != 2 {
		t.Fatalf("expected 2 vars, got %d", len(w.Vars))
	}

	gv := w.Vars["global_var"]
	if gv == nil {
		t.Fatal("global_var not found")
	}
	if gv.Type != VarString {
		t.Errorf("global_var type: expected string, got %v", gv.Type)
	}
	if !gv.HasDefault || gv.Default != "overridden" {
		t.Errorf("global_var default: expected 'overridden', got %v", gv.Default)
	}

	lv := w.Vars["local_var"]
	if lv == nil {
		t.Fatal("local_var not found")
	}
	if lv.Type != VarInt {
		t.Errorf("local_var type: expected int, got %v", lv.Type)
	}
	if !lv.HasDefault || lv.Default != int64(42) {
		t.Errorf("local_var default: expected 42, got %v", lv.Default)
	}
}

// ---------------------------------------------------------------------------
// Budget
// ---------------------------------------------------------------------------

const budgetSrc = `
schema s:
  ok: bool

prompt sys:
  Sys.

prompt usr:
  Usr.

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow budget_test:
  entry: a
  budget:
    max_parallel_branches: 4
    max_duration: "60m"
    max_cost_usd: 30
    max_tokens: 800000
  a -> done
`

func TestCompileBudget(t *testing.T) {
	w := mustCompile(t, budgetSrc)

	if w.Budget == nil {
		t.Fatal("expected budget")
	}
	if w.Budget.MaxParallelBranches != 4 {
		t.Errorf("max_parallel_branches: expected 4, got %d", w.Budget.MaxParallelBranches)
	}
	if w.Budget.MaxDuration != "60m" {
		t.Errorf("max_duration: expected '60m', got %q", w.Budget.MaxDuration)
	}
	if w.Budget.MaxCostUSD != 30 {
		t.Errorf("max_cost_usd: expected 30, got %v", w.Budget.MaxCostUSD)
	}
	if w.Budget.MaxTokens != 800000 {
		t.Errorf("max_tokens: expected 800000, got %d", w.Budget.MaxTokens)
	}
}

// ---------------------------------------------------------------------------
// Error diagnostics
// ---------------------------------------------------------------------------

func TestCompileNoWorkflow(t *testing.T) {
	src := `
schema s:
  ok: bool
`
	r := compileFile(t, src)
	if !r.HasErrors() {
		t.Fatal("expected error for missing workflow")
	}
	found := false
	for _, d := range r.Diagnostics {
		if d.Code == DiagNoWorkflow {
			found = true
		}
	}
	if !found {
		t.Error("expected DiagNoWorkflow diagnostic")
	}
}

func TestCompileUnknownSchemaRef(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  Sys.

prompt usr:
  Usr.

agent a:
  model: "m"
  input: nonexistent
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> done
`
	r := compileFile(t, src)
	if !r.HasErrors() {
		t.Fatal("expected error for unknown schema")
	}
	found := false
	for _, d := range r.Diagnostics {
		if d.Code == DiagUnknownSchema {
			found = true
		}
	}
	if !found {
		t.Error("expected DiagUnknownSchema diagnostic")
	}
}

func TestCompileUnknownEdgeTarget(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  Sys.

prompt usr:
  Usr.

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> nonexistent
`
	r := compileFile(t, src)
	if !r.HasErrors() {
		t.Fatal("expected error for unknown edge target")
	}
	found := false
	for _, d := range r.Diagnostics {
		if d.Code == DiagUnknownNode {
			found = true
		}
	}
	if !found {
		t.Error("expected DiagUnknownNode diagnostic")
	}
}

// ---------------------------------------------------------------------------
// Prompt template refs
// ---------------------------------------------------------------------------

func TestCompilePromptTemplateRefs(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  You are reviewing {{input.pr_context}} with {{vars.rules}}.

prompt usr:
  Do it.

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> done
`
	w := mustCompile(t, src)

	p := w.Prompts["sys"]
	if len(p.TemplateRefs) != 2 {
		t.Fatalf("expected 2 template refs in prompt, got %d", len(p.TemplateRefs))
	}
	if p.TemplateRefs[0].Kind != RefInput {
		t.Errorf("ref[0]: expected RefInput, got %v", p.TemplateRefs[0].Kind)
	}
	if p.TemplateRefs[1].Kind != RefVars {
		t.Errorf("ref[1]: expected RefVars, got %v", p.TemplateRefs[1].Kind)
	}
}

// ---------------------------------------------------------------------------
// Golden test: reference fixture compilation
// ---------------------------------------------------------------------------

func TestCompileReferenceFixture(t *testing.T) {
	fixtures := []string{
		"pr_refine_single_model.iter",
		"pr_refine_dual_model_parallel.iter",
		"pr_refine_dual_model_parallel_compliance.iter",
		"recipe_benchmark.iter",
		"ci_fix_until_green.iter",
	}

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			path := filepath.Join("..", "examples", fixture)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Skipf("fixture not found: %v", err)
			}

			res := parser.Parse(fixture, string(src))
			for _, d := range res.Diagnostics {
				if d.Severity == parser.SeverityError {
					t.Fatalf("parse error: %s", d.Error())
				}
			}

			cr := Compile(res.File)
			for _, d := range cr.Diagnostics {
				if d.Severity == SeverityError {
					t.Errorf("compile error: %s", d.Error())
				}
			}

			if cr.Workflow == nil {
				t.Fatal("expected non-nil workflow")
			}

			w := cr.Workflow

			// Basic sanity checks for all fixtures.
			if w.Name == "" {
				t.Error("workflow name is empty")
			}
			if w.Entry == "" {
				t.Error("workflow entry is empty")
			}
			if _, ok := w.Nodes[w.Entry]; !ok {
				t.Errorf("entry node %q not in nodes map", w.Entry)
			}
			if len(w.Edges) == 0 {
				t.Error("workflow has no edges")
			}
			if _, ok := w.Nodes["done"]; !ok {
				t.Error("terminal node 'done' missing")
			}
			if _, ok := w.Nodes["fail"]; !ok {
				t.Error("terminal node 'fail' missing")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Determinism: compiling twice yields identical IR
// ---------------------------------------------------------------------------

func TestCompileDeterminism(t *testing.T) {
	path := filepath.Join("..", "examples", "pr_refine_dual_model_parallel_compliance.iter")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("fixture not found: %v", err)
	}

	res1 := parser.Parse("test.iter", string(src))
	res2 := parser.Parse("test.iter", string(src))

	cr1 := Compile(res1.File)
	cr2 := Compile(res2.File)

	w1 := cr1.Workflow
	w2 := cr2.Workflow

	if w1.Name != w2.Name {
		t.Errorf("names differ: %q vs %q", w1.Name, w2.Name)
	}
	if w1.Entry != w2.Entry {
		t.Errorf("entries differ: %q vs %q", w1.Entry, w2.Entry)
	}
	if len(w1.Nodes) != len(w2.Nodes) {
		t.Errorf("node counts differ: %d vs %d", len(w1.Nodes), len(w2.Nodes))
	}
	if len(w1.Edges) != len(w2.Edges) {
		t.Errorf("edge counts differ: %d vs %d", len(w1.Edges), len(w2.Edges))
	}
	if len(w1.Schemas) != len(w2.Schemas) {
		t.Errorf("schema counts differ: %d vs %d", len(w1.Schemas), len(w2.Schemas))
	}
	if len(w1.Prompts) != len(w2.Prompts) {
		t.Errorf("prompt counts differ: %d vs %d", len(w1.Prompts), len(w2.Prompts))
	}
	if len(w1.Vars) != len(w2.Vars) {
		t.Errorf("var counts differ: %d vs %d", len(w1.Vars), len(w2.Vars))
	}
	if len(w1.Loops) != len(w2.Loops) {
		t.Errorf("loop counts differ: %d vs %d", len(w1.Loops), len(w2.Loops))
	}

	// Check edges are in the same order.
	for i := range w1.Edges {
		if w1.Edges[i].From != w2.Edges[i].From || w1.Edges[i].To != w2.Edges[i].To {
			t.Errorf("edge %d differs: %s->%s vs %s->%s",
				i, w1.Edges[i].From, w1.Edges[i].To, w2.Edges[i].From, w2.Edges[i].To)
		}
	}
}

// ---------------------------------------------------------------------------
// Detailed assertions on the reference fixture
// ---------------------------------------------------------------------------

func TestCompileReferenceFixtureDetailed(t *testing.T) {
	path := filepath.Join("..", "examples", "pr_refine_dual_model_parallel_compliance.iter")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("fixture not found: %v", err)
	}

	res := parser.Parse("test.iter", string(src))
	cr := Compile(res.File)

	if cr.HasErrors() {
		for _, d := range cr.Diagnostics {
			t.Errorf("diagnostic: %s", d.Error())
		}
		t.Fatal("reference fixture must compile without errors")
	}

	w := cr.Workflow

	// Workflow metadata
	if w.Name != "pr_refine_dual_model_parallel_compliance" {
		t.Errorf("name: expected pr_refine_dual_model_parallel_compliance, got %q", w.Name)
	}
	if w.Entry != "context_builder" {
		t.Errorf("entry: expected context_builder, got %q", w.Entry)
	}

	// Node counts: 20 declared + done + fail = 22
	// Count declared nodes from fixture:
	// agents: context_builder, claude_review, gpt_review, claude_plan, gpt_plan,
	//         claude_plan_synthesis, gpt_plan_synthesis, final_plan_merge,
	//         integrate_human_clarifications, refine_plan_claude, refine_plan_gpt,
	//         act_on_plan, claude_final_review, gpt_final_review = 14
	// judges: plan_compliance_check_initial, technical_decision_gate,
	//         plan_compliance_check_post_human, plan_compliance_check_after_claude,
	//         plan_compliance_check_after_gpt, final_pr_compliance_check = 6
	// routers: initial_review_fanout, plan_synthesis_fanout, final_review_fanout = 3
	// joins: initial_plans_join, plan_syntheses_join, final_reviews_join = 3
	// humans: technical_decision_human_checkpoint = 1
	// Total declared: 14+6+3+3+1 = 27
	// + done + fail = 29
	expectedNodeCount := 29
	if len(w.Nodes) != expectedNodeCount {
		t.Errorf("node count: expected %d, got %d", expectedNodeCount, len(w.Nodes))
	}

	// Loops
	if len(w.Loops) != 2 {
		t.Errorf("loop count: expected 2, got %d", len(w.Loops))
	}
	if loop, ok := w.Loops["plan_refine_loop"]; ok {
		if loop.MaxIterations != 6 {
			t.Errorf("plan_refine_loop max_iterations: expected 6, got %d", loop.MaxIterations)
		}
	} else {
		t.Error("loop 'plan_refine_loop' not found")
	}
	if loop, ok := w.Loops["full_recipe_loop"]; ok {
		if loop.MaxIterations != 3 {
			t.Errorf("full_recipe_loop max_iterations: expected 3, got %d", loop.MaxIterations)
		}
	} else {
		t.Error("loop 'full_recipe_loop' not found")
	}

	// Budget
	if w.Budget == nil {
		t.Fatal("expected budget")
	}
	if w.Budget.MaxParallelBranches != 4 {
		t.Errorf("budget max_parallel_branches: expected 4, got %d", w.Budget.MaxParallelBranches)
	}
	if w.Budget.MaxCostUSD != 30 {
		t.Errorf("budget max_cost_usd: expected 30, got %v", w.Budget.MaxCostUSD)
	}

	// Vars
	if len(w.Vars) < 4 {
		t.Errorf("vars count: expected at least 4, got %d", len(w.Vars))
	}

	// Specific node checks
	cb := w.Nodes["context_builder"]
	if cb.Kind != NodeAgent {
		t.Errorf("context_builder should be agent")
	}
	if cb.Publish != "pr_context" {
		t.Errorf("context_builder publish: expected 'pr_context', got %q", cb.Publish)
	}
	if len(cb.Tools) != 6 {
		t.Errorf("context_builder tools: expected 6, got %d", len(cb.Tools))
	}

	// Router
	irf := w.Nodes["initial_review_fanout"]
	if irf.Kind != NodeRouter || irf.RouterMode != RouterFanOutAll {
		t.Errorf("initial_review_fanout: expected router fan_out_all")
	}

	// Join
	ipj := w.Nodes["initial_plans_join"]
	if ipj.Kind != NodeJoin || ipj.JoinStrategy != JoinWaitAll {
		t.Errorf("initial_plans_join: expected join wait_all")
	}

	// Human
	hc := w.Nodes["technical_decision_human_checkpoint"]
	if hc.Kind != NodeHuman {
		t.Errorf("technical_decision_human_checkpoint: expected human")
	}
	if hc.MinAnswers != 1 {
		t.Errorf("human min_answers: expected 1, got %d", hc.MinAnswers)
	}

	// Edge with data mappings: check a with-bearing edge has parsed refs
	found := false
	for _, e := range w.Edges {
		if e.From == "context_builder" && e.To == "initial_review_fanout" {
			found = true
			if len(e.With) != 1 {
				t.Errorf("context_builder->initial_review_fanout: expected 1 with mapping, got %d", len(e.With))
			} else if e.With[0].Key != "pr_context" {
				t.Errorf("with key: expected 'pr_context', got %q", e.With[0].Key)
			} else if len(e.With[0].Refs) != 1 || e.With[0].Refs[0].Kind != RefOutputs {
				t.Errorf("with ref: expected RefOutputs")
			}
			break
		}
	}
	if !found {
		t.Error("edge context_builder->initial_review_fanout not found")
	}
}

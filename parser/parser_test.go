package parser_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/ast"
	"github.com/SocialGouv/iterion/parser"
)

// ---------------------------------------------------------------------------
// Golden tests — all reference fixtures must parse without errors
// ---------------------------------------------------------------------------

func TestFixtures(t *testing.T) {
	fixtures, err := filepath.Glob("../examples/*.iter")
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no .iter fixtures found in ../examples/")
	}

	for _, path := range fixtures {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			res := parser.Parse(name, string(src))
			if len(res.Diagnostics) > 0 {
				for _, d := range res.Diagnostics {
					t.Errorf("diagnostic: %s", d.Error())
				}
			}
			if res.File == nil {
				t.Fatal("parsed file is nil")
			}
		})
	}
}

// TestFixturePRRefineSingleModel validates detailed AST structure.
func TestFixturePRRefineSingleModel(t *testing.T) {
	src := readFixture(t, "../examples/pr_refine_single_model.iter")
	res := parser.Parse("pr_refine_single_model.iter", src)
	assertNoDiags(t, res)

	f := res.File

	// vars
	if f.Vars == nil {
		t.Fatal("expected top-level vars block")
	}
	if len(f.Vars.Fields) != 1 {
		t.Fatalf("expected 1 var field, got %d", len(f.Vars.Fields))
	}
	assertEq(t, "vars[0].Name", f.Vars.Fields[0].Name, "workspace_dir")
	assertEq(t, "vars[0].Type", f.Vars.Fields[0].Type.String(), "string")
	if f.Vars.Fields[0].Default == nil {
		t.Fatal("expected default value for workspace_dir")
	}
	assertEq(t, "vars[0].Default", f.Vars.Fields[0].Default.StrVal, "${PROJECT_DIR}")

	// prompts
	if len(f.Prompts) != 12 {
		t.Fatalf("expected 12 prompts, got %d", len(f.Prompts))
	}
	assertEq(t, "prompts[0].Name", f.Prompts[0].Name, "review_system")

	// schemas
	if len(f.Schemas) != 11 {
		t.Fatalf("expected 11 schemas, got %d", len(f.Schemas))
	}

	// agents
	if len(f.Agents) != 5 {
		t.Fatalf("expected 5 agents, got %d", len(f.Agents))
	}
	assertEq(t, "agents[0].Name", f.Agents[0].Name, "context_builder")
	assertEq(t, "agents[0].Model", f.Agents[0].Model, "${MODEL}")
	assertEq(t, "agents[0].Input", f.Agents[0].Input, "repo_context_request")
	assertEq(t, "agents[0].Session", f.Agents[0].Session, ast.SessionFresh)

	// judges
	if len(f.Judges) != 3 {
		t.Fatalf("expected 3 judges, got %d", len(f.Judges))
	}

	// workflow
	if len(f.Workflows) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(f.Workflows))
	}
	wf := f.Workflows[0]
	assertEq(t, "workflow.Name", wf.Name, "pr_refine_single_model")
	assertEq(t, "workflow.Entry", wf.Entry, "context_builder")

	if wf.Budget == nil {
		t.Fatal("expected budget block")
	}
	assertEq(t, "budget.MaxParallelBranches", wf.Budget.MaxParallelBranches, 1)
	assertEq(t, "budget.MaxDuration", wf.Budget.MaxDuration, "30m")

	if len(wf.Edges) < 10 {
		t.Fatalf("expected at least 10 edges, got %d", len(wf.Edges))
	}
}

// TestFixtureCIFixUntilGreen validates the CI fix fixture structure.
func TestFixtureCIFixUntilGreen(t *testing.T) {
	src := readFixture(t, "../examples/ci_fix_until_green.iter")
	res := parser.Parse("ci_fix_until_green.iter", src)
	assertNoDiags(t, res)

	f := res.File

	// Check tool node
	if len(f.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(f.Tools))
	}
	assertEq(t, "tool.Name", f.Tools[0].Name, "run_ci")
	assertEq(t, "tool.Command", f.Tools[0].Command, "${CI_COMMAND}")

	// Check workflow with loop
	wf := f.Workflows[0]
	var loopEdge *ast.Edge
	for _, e := range wf.Edges {
		if e.Loop != nil {
			loopEdge = e
			break
		}
	}
	if loopEdge == nil {
		t.Fatal("expected at least one edge with loop clause")
	}
	assertEq(t, "loop.Name", loopEdge.Loop.Name, "fix_loop")
	assertEq(t, "loop.MaxIterations", loopEdge.Loop.MaxIterations, 5)
}

// TestFixtureRecipeBenchmark validates router/join/fan-out patterns.
func TestFixtureRecipeBenchmark(t *testing.T) {
	src := readFixture(t, "../examples/recipe_benchmark.iter")
	res := parser.Parse("recipe_benchmark.iter", src)
	assertNoDiags(t, res)

	f := res.File

	// Router
	if len(f.Routers) != 1 {
		t.Fatalf("expected 1 router, got %d", len(f.Routers))
	}
	assertEq(t, "router.Name", f.Routers[0].Name, "recipe_fanout")
	assertEq(t, "router.Mode", f.Routers[0].Mode, ast.RouterFanOutAll)

	// Join
	if len(f.Joins) != 1 {
		t.Fatalf("expected 1 join, got %d", len(f.Joins))
	}
	assertEq(t, "join.Name", f.Joins[0].Name, "recipes_join")
	assertEq(t, "join.Strategy", f.Joins[0].Strategy, ast.JoinWaitAll)
	if len(f.Joins[0].Require) != 2 {
		t.Fatalf("expected 2 require entries, got %d", len(f.Joins[0].Require))
	}
}

// ---------------------------------------------------------------------------
// Minimal valid input tests
// ---------------------------------------------------------------------------

func TestMinimalAgent(t *testing.T) {
	src := `agent greeter:
  model: "gpt-4"
  input: in_schema
  output: out_schema
  system: sys_prompt
  user: usr_prompt
  session: fresh
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	if len(res.File.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(res.File.Agents))
	}
	a := res.File.Agents[0]
	assertEq(t, "Name", a.Name, "greeter")
	assertEq(t, "Model", a.Model, "gpt-4")
	assertEq(t, "Session", a.Session, ast.SessionFresh)
}

func TestMinimalWorkflow(t *testing.T) {
	src := `workflow simple:
  entry: start

  start -> done
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	wf := res.File.Workflows[0]
	assertEq(t, "Name", wf.Name, "simple")
	assertEq(t, "Entry", wf.Entry, "start")
	if len(wf.Edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(wf.Edges))
	}
	assertEq(t, "Edge.From", wf.Edges[0].From, "start")
	assertEq(t, "Edge.To", wf.Edges[0].To, "done")
}

func TestPromptBody(t *testing.T) {
	src := `prompt greeting:
  Bonjour {{input.name}},
  Comment allez-vous ?
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	if len(res.File.Prompts) != 1 {
		t.Fatalf("expected 1 prompt, got %d", len(res.File.Prompts))
	}
	body := res.File.Prompts[0].Body
	if !strings.Contains(body, "Bonjour {{input.name}},") {
		t.Errorf("expected prompt body to contain template expression, got: %s", body)
	}
	if !strings.Contains(body, "Comment allez-vous ?") {
		t.Errorf("expected multi-line prompt body, got: %s", body)
	}
}

func TestSchemaWithEnum(t *testing.T) {
	src := `schema verdict:
  approved: bool
  confidence: string [enum: "low", "medium", "high"]
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	s := res.File.Schemas[0]
	if len(s.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(s.Fields))
	}
	assertEq(t, "field[0].Type", s.Fields[0].Type, ast.FieldTypeBool)
	if len(s.Fields[1].EnumValues) != 3 {
		t.Fatalf("expected 3 enum values, got %d", len(s.Fields[1].EnumValues))
	}
	assertEq(t, "enum[0]", s.Fields[1].EnumValues[0], "low")
	assertEq(t, "enum[2]", s.Fields[1].EnumValues[2], "high")
}

func TestVarsWithDefaults(t *testing.T) {
	src := `vars:
  name: string = "default"
  count: int = 10
  rate: float = 3.14
  debug: bool = false
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	vb := res.File.Vars
	if vb == nil || len(vb.Fields) != 4 {
		t.Fatalf("expected 4 var fields, got %v", vb)
	}
	assertEq(t, "name.Default", vb.Fields[0].Default.StrVal, "default")
	assertEq(t, "count.Default", vb.Fields[1].Default.IntVal, int64(10))
	assertEq(t, "rate.Default", vb.Fields[2].Default.FloatVal, 3.14)
	assertEq(t, "debug.Default", vb.Fields[3].Default.BoolVal, false)
}

func TestEdgeWithAllClauses(t *testing.T) {
	src := `workflow test:
  entry: a

  a -> b when approved as loop1(3) with {
    key1: "{{outputs.a}}",
    key2: "{{vars.x}}"
  }
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	e := res.File.Workflows[0].Edges[0]
	assertEq(t, "From", e.From, "a")
	assertEq(t, "To", e.To, "b")

	if e.When == nil {
		t.Fatal("expected when clause")
	}
	assertEq(t, "When.Condition", e.When.Condition, "approved")
	assertEq(t, "When.Negated", e.When.Negated, false)

	if e.Loop == nil {
		t.Fatal("expected loop clause")
	}
	assertEq(t, "Loop.Name", e.Loop.Name, "loop1")
	assertEq(t, "Loop.Max", e.Loop.MaxIterations, 3)

	if len(e.With) != 2 {
		t.Fatalf("expected 2 with entries, got %d", len(e.With))
	}
	assertEq(t, "with[0].Key", e.With[0].Key, "key1")
	assertEq(t, "with[0].Value", e.With[0].Value, "{{outputs.a}}")
}

func TestEdgeWhenNot(t *testing.T) {
	src := `workflow test:
  entry: a

  a -> b when not approved
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	e := res.File.Workflows[0].Edges[0]
	if e.When == nil || !e.When.Negated {
		t.Fatal("expected negated when clause")
	}
	assertEq(t, "When.Condition", e.When.Condition, "approved")
}

func TestToolNode(t *testing.T) {
	src := `tool run_ci:
  command: "${CI_COMMAND}"
  output: ci_result
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	td := res.File.Tools[0]
	assertEq(t, "Name", td.Name, "run_ci")
	assertEq(t, "Command", td.Command, "${CI_COMMAND}")
	assertEq(t, "Output", td.Output, "ci_result")
}

func TestJoinDecl(t *testing.T) {
	src := `join sync:
  strategy: wait_all
  require: [node_a, node_b, node_c]
  output: merged
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	j := res.File.Joins[0]
	assertEq(t, "Strategy", j.Strategy, ast.JoinWaitAll)
	if len(j.Require) != 3 {
		t.Fatalf("expected 3 require, got %d", len(j.Require))
	}
	assertEq(t, "Output", j.Output, "merged")
}

func TestRouterDecl(t *testing.T) {
	src := `router dispatch:
  mode: condition
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	r := res.File.Routers[0]
	assertEq(t, "Mode", r.Mode, ast.RouterCondition)
}

func TestRouterDeclRoundRobin(t *testing.T) {
	src := `router selector:
  mode: round_robin
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	r := res.File.Routers[0]
	assertEq(t, "Name", r.Name, "selector")
	assertEq(t, "Mode", r.Mode, ast.RouterRoundRobin)
}

func TestHumanDecl(t *testing.T) {
	src := `human review:
  input: review_in
  output: review_out
  instructions: review_prompt
  mode: pause_until_answers
  min_answers: 2
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	h := res.File.Humans[0]
	assertEq(t, "Mode", h.Mode, ast.HumanPauseUntilAnswers)
	assertEq(t, "MinAnswers", h.MinAnswers, 2)
}

func TestAgentWithTools(t *testing.T) {
	src := `agent worker:
  model: "claude-4"
  input: in_s
  output: out_s
  system: sys
  user: usr
  session: inherit
  tools: [read_file, write_file, git_diff]
  tool_max_steps: 15
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	a := res.File.Agents[0]
	assertEq(t, "Session", a.Session, ast.SessionInherit)
	if len(a.Tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(a.Tools))
	}
	assertEq(t, "ToolMaxSteps", a.ToolMaxSteps, 15)
}

func TestAgentSessionFork(t *testing.T) {
	src := `agent commit_namer:
  model: "claude-4"
  delegate: "claude_code"
  input: in_s
  output: out_s
  system: sys
  user: usr
  session: fork
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	a := res.File.Agents[0]
	assertEq(t, "Session", a.Session, ast.SessionFork)
	assertEq(t, "Delegate", a.Delegate, "claude_code")
}

func TestAgentReasoningEffort(t *testing.T) {
	src := `agent planner:
  model: "claude-4"
  reasoning_effort: high
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)
	assertEq(t, "ReasoningEffort", res.File.Agents[0].ReasoningEffort, "high")
}

func TestAgentReasoningEffortExtraHigh(t *testing.T) {
	src := `agent planner:
  model: "claude-4"
  reasoning_effort: extra_high
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)
	assertEq(t, "ReasoningEffort", res.File.Agents[0].ReasoningEffort, "extra_high")
}

func TestJudgeReasoningEffort(t *testing.T) {
	src := `judge reviewer:
  model: "claude-4"
  reasoning_effort: low
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)
	assertEq(t, "ReasoningEffort", res.File.Judges[0].ReasoningEffort, "low")
}

func TestReasoningEffortInvalid(t *testing.T) {
	src := `agent planner:
  model: "claude-4"
  reasoning_effort: ultra
`
	res := parser.Parse("test.iter", src)
	if len(res.Diagnostics) == 0 {
		t.Fatal("expected diagnostic for invalid reasoning_effort")
	}
}

func TestComments(t *testing.T) {
	src := `## Top-level comment
agent x:
  model: "m"
  input: i
  output: o
  system: s
  user: u
  session: fresh
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)
	if len(res.File.Comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(res.File.Comments))
	}
}

func TestBudgetBlock(t *testing.T) {
	src := `workflow w:
  entry: a
  budget:
    max_parallel_branches: 2
    max_duration: "60m"
    max_cost_usd: 30.5
    max_tokens: 400000
    max_iterations: 10
  a -> done
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	b := res.File.Workflows[0].Budget
	if b == nil {
		t.Fatal("expected budget")
	}
	assertEq(t, "MaxParallel", b.MaxParallelBranches, 2)
	assertEq(t, "MaxDuration", b.MaxDuration, "60m")
	assertEq(t, "MaxCostUSD", b.MaxCostUSD, 30.5)
	assertEq(t, "MaxTokens", b.MaxTokens, 400000)
	assertEq(t, "MaxIterations", b.MaxIterations, 10)
}

func TestWorkflowVars(t *testing.T) {
	src := `workflow w:
  vars:
    name: string
    count: int = 5
  entry: a
  a -> done
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	wv := res.File.Workflows[0].Vars
	if wv == nil || len(wv.Fields) != 2 {
		t.Fatalf("expected 2 workflow vars, got %v", wv)
	}
	assertEq(t, "var[0].Name", wv.Fields[0].Name, "name")
	if wv.Fields[0].Default != nil {
		t.Error("first var should have no default")
	}
	assertEq(t, "var[1].Default.IntVal", wv.Fields[1].Default.IntVal, int64(5))
}

// ---------------------------------------------------------------------------
// Error / diagnostic tests
// ---------------------------------------------------------------------------

func TestDiagReservedName(t *testing.T) {
	src := `agent done:
  model: "m"
  input: i
  output: o
  system: s
  user: u
  session: fresh
`
	res := parser.Parse("test.iter", src)
	if !hasDiagCode(res, parser.DiagReservedName) {
		t.Error("expected DiagReservedName for agent named 'done'")
	}
}

func TestDiagBadIndentation(t *testing.T) {
	src := `agent foo:
  model: "m"
   input: i
  output: o
  system: s
  user: u
  session: fresh
`
	res := parser.Parse("test.iter", src)
	// 3-space indent should produce an indentation error
	if !hasDiagCode(res, parser.DiagBadIndentation) && !hasDiagCode(res, parser.DiagExpectedToken) && !hasDiagCode(res, parser.DiagUnexpectedToken) {
		// The lexer emits TokenError for bad indentation which the parser sees
		// It's acceptable to get any structural error here
		hasAnyError := len(res.Diagnostics) > 0
		if !hasAnyError {
			t.Error("expected diagnostic for misaligned indentation")
		}
	}
}

func TestDiagUnknownProperty(t *testing.T) {
	src := `agent foo:
  model: "m"
  input: i
  output: o
  system: s
  user: u
  session: fresh
  foobar: baz
`
	res := parser.Parse("test.iter", src)
	if !hasDiagCode(res, parser.DiagUnknownProperty) {
		t.Error("expected DiagUnknownProperty for 'foobar'")
	}
}

func TestDiagInvalidSessionMode(t *testing.T) {
	src := `agent foo:
  model: "m"
  input: i
  output: o
  system: s
  user: u
  session: invalid_mode
`
	res := parser.Parse("test.iter", src)
	if !hasDiagCode(res, parser.DiagInvalidValue) {
		t.Error("expected DiagInvalidValue for invalid session mode")
	}
}

func TestDiagInvalidType(t *testing.T) {
	src := `vars:
  x: foobar
`
	res := parser.Parse("test.iter", src)
	if !hasDiagCode(res, parser.DiagInvalidType) {
		t.Error("expected DiagInvalidType for unknown type 'foobar'")
	}
}

func TestDiagUnexpectedTopLevel(t *testing.T) {
	src := `12345
`
	res := parser.Parse("test.iter", src)
	if len(res.Diagnostics) == 0 {
		t.Error("expected diagnostic for unexpected top-level token")
	}
}

func TestMultipleDeclarations(t *testing.T) {
	src := `schema a:
  x: string

schema b:
  y: int

agent c:
  model: "m"
  input: a
  output: b
  system: s
  user: u
  session: fresh

workflow w:
  entry: c
  c -> done
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	assertEq(t, "schemas", len(res.File.Schemas), 2)
	assertEq(t, "agents", len(res.File.Agents), 1)
	assertEq(t, "workflows", len(res.File.Workflows), 1)
}

func TestAllFieldTypes(t *testing.T) {
	src := `schema all_types:
  a: string
  b: bool
  c: int
  d: float
  e: json
  f: string[]
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	fields := res.File.Schemas[0].Fields
	if len(fields) != 6 {
		t.Fatalf("expected 6 fields, got %d", len(fields))
	}
	assertEq(t, "string", fields[0].Type, ast.FieldTypeString)
	assertEq(t, "bool", fields[1].Type, ast.FieldTypeBool)
	assertEq(t, "int", fields[2].Type, ast.FieldTypeInt)
	assertEq(t, "float", fields[3].Type, ast.FieldTypeFloat)
	assertEq(t, "json", fields[4].Type, ast.FieldTypeJSON)
	assertEq(t, "string[]", fields[5].Type, ast.FieldTypeStringArray)
}

// ---------------------------------------------------------------------------
// Lexer-level tests
// ---------------------------------------------------------------------------

func TestLexerBasic(t *testing.T) {
	src := `agent foo:
  model: "gpt-4"
`
	lex := parser.NewLexer("test.iter", src)
	tokens := lex.All()

	// Verify key tokens are present
	var types []parser.TokenType
	for _, tok := range tokens {
		types = append(types, tok.Type)
	}

	expected := []parser.TokenType{
		parser.TokenAgent,
		parser.TokenIdent, // foo
		parser.TokenColon,
		parser.TokenNewline,
		parser.TokenIndent,
		parser.TokenModel,
		parser.TokenColon,
		parser.TokenString, // gpt-4
		parser.TokenNewline,
		parser.TokenDedent,
		parser.TokenEOF,
	}

	if len(types) != len(expected) {
		t.Fatalf("expected %d tokens, got %d: %v", len(expected), len(types), types)
	}
	for i, exp := range expected {
		if types[i] != exp {
			t.Errorf("token[%d]: expected %s, got %s", i, exp, types[i])
		}
	}
}

func TestLexerIndentDedent(t *testing.T) {
	src := `a:
  b:
    c
  d
e
`
	lex := parser.NewLexer("test.iter", src)
	tokens := lex.All()

	var types []parser.TokenType
	for _, tok := range tokens {
		types = append(types, tok.Type)
	}

	// Expected: a : NL INDENT b : NL INDENT c NL DEDENT d NL DEDENT e NL EOF
	indents := 0
	dedents := 0
	for _, tt := range types {
		if tt == parser.TokenIndent {
			indents++
		}
		if tt == parser.TokenDedent {
			dedents++
		}
	}
	if indents != 2 {
		t.Errorf("expected 2 INDENTs, got %d", indents)
	}
	if dedents != 2 {
		t.Errorf("expected 2 DEDENTs, got %d", dedents)
	}
}

func TestLexerPromptMode(t *testing.T) {
	src := `prompt hello:
  Bonjour {{input.name}},
  Comment allez-vous ?
agent foo:
  model: "m"
`
	lex := parser.NewLexer("test.iter", src)
	tokens := lex.All()

	promptLines := 0
	for _, tok := range tokens {
		if tok.Type == parser.TokenPromptLine {
			promptLines++
		}
	}
	if promptLines != 2 {
		t.Errorf("expected 2 prompt lines, got %d", promptLines)
	}
}

func TestLexerComment(t *testing.T) {
	src := `## This is a comment
agent foo:
  model: "m"
`
	lex := parser.NewLexer("test.iter", src)
	tokens := lex.All()

	found := false
	for _, tok := range tokens {
		if tok.Type == parser.TokenComment {
			found = true
			assertEq(t, "comment", tok.Value, "This is a comment")
		}
	}
	if !found {
		t.Error("expected a comment token")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func readFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertNoDiags(t *testing.T, res *parser.ParseResult) {
	t.Helper()
	if len(res.Diagnostics) > 0 {
		for _, d := range res.Diagnostics {
			t.Errorf("unexpected diagnostic: %s", d.Error())
		}
		t.FailNow()
	}
}

func hasDiagCode(res *parser.ParseResult, code parser.DiagCode) bool {
	for _, d := range res.Diagnostics {
		if d.Code == code {
			return true
		}
	}
	return false
}

func assertEq[T comparable](t *testing.T, label string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", label, got, want)
	}
}

// Verify fixtures produce stable AST (re-parsing produces identical structure).
func TestFixtureStability(t *testing.T) {
	fixtures, err := filepath.Glob("../examples/*.iter")
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range fixtures {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			srcStr := string(src)
			res1 := parser.Parse(name, srcStr)
			res2 := parser.Parse(name, srcStr)

			assertNoDiags(t, res1)
			assertNoDiags(t, res2)

			// Compare key structural properties
			f1 := res1.File
			f2 := res2.File
			assertEq(t, "prompts", len(f1.Prompts), len(f2.Prompts))
			assertEq(t, "schemas", len(f1.Schemas), len(f2.Schemas))
			assertEq(t, "agents", len(f1.Agents), len(f2.Agents))
			assertEq(t, "judges", len(f1.Judges), len(f2.Judges))
			assertEq(t, "routers", len(f1.Routers), len(f2.Routers))
			assertEq(t, "joins", len(f1.Joins), len(f2.Joins))
			assertEq(t, "humans", len(f1.Humans), len(f2.Humans))
			assertEq(t, "tools", len(f1.Tools), len(f2.Tools))
			assertEq(t, "workflows", len(f1.Workflows), len(f2.Workflows))

			for i, w := range f1.Workflows {
				assertEq(t, "workflow.Name", w.Name, f2.Workflows[i].Name)
				assertEq(t, "workflow.Entry", w.Entry, f2.Workflows[i].Entry)
				assertEq(t, "workflow.Edges", len(w.Edges), len(f2.Workflows[i].Edges))
			}
		})
	}
}

func TestAgentDelegate(t *testing.T) {
	src := `agent worker:
  delegate: "claude_code"
  input: in_s
  output: out_s
  system: sys
  user: usr
  session: fresh
  tools: [read_file, write_file]
  tool_max_steps: 10
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	a := res.File.Agents[0]
	assertEq(t, "Delegate", a.Delegate, "claude_code")
	assertEq(t, "Model", a.Model, "")
	if len(a.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(a.Tools))
	}
}

func TestJudgeDelegate(t *testing.T) {
	src := `judge verdict:
  delegate: "codex"
  input: in_s
  output: out_s
  system: sys
  user: usr
  session: fresh
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	j := res.File.Judges[0]
	assertEq(t, "Delegate", j.Delegate, "codex")
	assertEq(t, "Model", j.Model, "")
}

func TestDottedToolNames(t *testing.T) {
	src := `agent worker:
  model: "claude-4"
  input: in_s
  output: out_s
  system: sys
  user: usr
  session: fresh
  tools: [git_diff, mcp.claude_code.search, mcp.falcon.lookup]
  tool_max_steps: 5
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	a := res.File.Agents[0]
	if len(a.Tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(a.Tools))
	}
	assertEq(t, "Tools[0]", a.Tools[0], "git_diff")
	assertEq(t, "Tools[1]", a.Tools[1], "mcp.claude_code.search")
	assertEq(t, "Tools[2]", a.Tools[2], "mcp.falcon.lookup")
}

func TestWildcardToolRef(t *testing.T) {
	src := `agent worker:
  model: "claude-4"
  input: in_s
  output: out_s
  system: sys
  user: usr
  session: fresh
  tools: [mcp.claude_code.*, git_diff, mcp.codex.*]
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	a := res.File.Agents[0]
	if len(a.Tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(a.Tools))
	}
	assertEq(t, "Tools[0]", a.Tools[0], "mcp.claude_code.*")
	assertEq(t, "Tools[1]", a.Tools[1], "git_diff")
	assertEq(t, "Tools[2]", a.Tools[2], "mcp.codex.*")
}

func TestMCPServerDecl(t *testing.T) {
	src := `mcp_server github:
  transport: http
  url: "https://api.githubcopilot.com/mcp"
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	if len(res.File.MCPServers) != 1 {
		t.Fatalf("expected 1 mcp_server, got %d", len(res.File.MCPServers))
	}
	server := res.File.MCPServers[0]
	assertEq(t, "Name", server.Name, "github")
	assertEq(t, "Transport", server.Transport, ast.MCPTransportHTTP)
	assertEq(t, "URL", server.URL, "https://api.githubcopilot.com/mcp")
}

func TestWorkflowAndNodeMCPBlocks(t *testing.T) {
	src := `mcp_server github:
  transport: http
  url: "https://example.com/mcp"

agent implement:
  model: "anthropic/claude-sonnet-4-6"
  mcp:
    inherit: true
    servers: [github]
    disable: [codex]
  input: in_s
  output: out_s
  system: sys
  user: usr

judge review:
  model: "openai/gpt-5"
  mcp:
    inherit: false
    servers: [claude_code]
  input: in_s
  output: out_s
  system: sys
  user: usr

workflow flow:
  entry: implement
  mcp:
    autoload_project: true
    servers: [claude_code, github]
    disable: [falcon]
  implement -> review
  review -> done
`
	res := parser.Parse("test.iter", src)
	assertNoDiags(t, res)

	wf := res.File.Workflows[0]
	if wf.MCP == nil {
		t.Fatal("expected workflow mcp block")
	}
	if wf.MCP.AutoloadProject == nil || !*wf.MCP.AutoloadProject {
		t.Fatal("expected autoload_project: true")
	}
	if len(wf.MCP.Servers) != 2 {
		t.Fatalf("expected 2 workflow mcp servers, got %d", len(wf.MCP.Servers))
	}
	assertEq(t, "workflow disable", wf.MCP.Disable[0], "falcon")

	agent := res.File.Agents[0]
	if agent.MCP == nil {
		t.Fatal("expected agent mcp block")
	}
	if agent.MCP.Inherit == nil || !*agent.MCP.Inherit {
		t.Fatal("expected inherit: true")
	}
	assertEq(t, "agent server", agent.MCP.Servers[0], "github")
	assertEq(t, "agent disable", agent.MCP.Disable[0], "codex")

	judge := res.File.Judges[0]
	if judge.MCP == nil {
		t.Fatal("expected judge mcp block")
	}
	if judge.MCP.Inherit == nil || *judge.MCP.Inherit {
		t.Fatal("expected inherit: false")
	}
	assertEq(t, "judge server", judge.MCP.Servers[0], "claude_code")
}

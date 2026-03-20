package ast_test

import (
	"testing"

	"github.com/iterion-ai/iterion/ast"
)

// TestMinimalWorkflow verifies that the AST can represent a minimal
// valid workflow: single agent → done.
func TestMinimalWorkflow(t *testing.T) {
	file := &ast.File{
		Prompts: []*ast.PromptDecl{
			{Name: "sys", Body: "Tu es un assistant."},
			{Name: "usr", Body: "Dis bonjour."},
		},
		Schemas: []*ast.SchemaDecl{
			{
				Name: "greeting_in",
				Fields: []*ast.SchemaField{
					{Name: "name", Type: ast.FieldTypeString},
				},
			},
			{
				Name: "greeting_out",
				Fields: []*ast.SchemaField{
					{Name: "message", Type: ast.FieldTypeString},
				},
			},
		},
		Agents: []*ast.AgentDecl{
			{
				Name:    "greeter",
				Model:   "${MODEL}",
				Input:   "greeting_in",
				Output:  "greeting_out",
				System:  "sys",
				User:    "usr",
				Session: ast.SessionFresh,
			},
		},
		Workflows: []*ast.WorkflowDecl{
			{
				Name:  "hello",
				Entry: "greeter",
				Edges: []*ast.Edge{
					{From: "greeter", To: "done"},
				},
			},
		},
	}

	// Verify node names
	names := file.AllNodeNames()
	if len(names) != 1 || names[0] != "greeter" {
		t.Fatalf("expected [greeter], got %v", names)
	}

	// Verify schema names
	schemas := file.AllSchemaNames()
	if len(schemas) != 2 {
		t.Fatalf("expected 2 schemas, got %d", len(schemas))
	}

	// Verify prompts
	prompts := file.AllPromptNames()
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(prompts))
	}

	// Verify edge target is a reserved terminal
	edge := file.Workflows[0].Edges[0]
	if !ast.ReservedTargets[edge.To] {
		t.Fatalf("expected 'done' to be a reserved target")
	}
}

// TestLoopEdge verifies the AST can represent conditional edges with loops.
func TestLoopEdge(t *testing.T) {
	edge := &ast.Edge{
		From: "compliance_check",
		To:   "refine_plan",
		When: &ast.WhenClause{
			Condition: "approved",
			Negated:   true,
		},
		Loop: &ast.LoopClause{
			Name:          "refine_loop",
			MaxIterations: 4,
		},
		With: []*ast.WithEntry{
			{Key: "plan", Value: `{{outputs.planner}}`},
			{Key: "verdict", Value: `{{outputs.compliance_check}}`},
		},
	}

	if edge.When == nil || !edge.When.Negated {
		t.Fatal("expected negated when clause")
	}
	if edge.Loop.MaxIterations != 4 {
		t.Fatalf("expected max 4, got %d", edge.Loop.MaxIterations)
	}
	if len(edge.With) != 2 {
		t.Fatalf("expected 2 with entries, got %d", len(edge.With))
	}
}

// TestParallelFanOutJoin verifies the AST can represent router → parallel agents → join.
func TestParallelFanOutJoin(t *testing.T) {
	file := &ast.File{
		Routers: []*ast.RouterDecl{
			{Name: "fanout", Mode: ast.RouterFanOutAll},
		},
		Agents: []*ast.AgentDecl{
			{Name: "worker_a", Model: "${MODEL_A}", Input: "req", Output: "res", System: "sys", User: "usr", Session: ast.SessionFresh},
			{Name: "worker_b", Model: "${MODEL_B}", Input: "req", Output: "res", System: "sys", User: "usr", Session: ast.SessionFresh},
		},
		Joins: []*ast.JoinDecl{
			{
				Name:     "sync",
				Strategy: ast.JoinWaitAll,
				Require:  []string{"worker_a", "worker_b"},
				Output:   "bundle",
			},
		},
		Workflows: []*ast.WorkflowDecl{
			{
				Name:  "parallel_demo",
				Entry: "fanout",
				Edges: []*ast.Edge{
					{From: "fanout", To: "worker_a", With: []*ast.WithEntry{{Key: "data", Value: "{{input.data}}"}}},
					{From: "fanout", To: "worker_b", With: []*ast.WithEntry{{Key: "data", Value: "{{input.data}}"}}},
					{From: "worker_a", To: "sync"},
					{From: "worker_b", To: "sync"},
					{From: "sync", To: "done"},
				},
			},
		},
	}

	nodes := file.AllNodeNames()
	if len(nodes) != 4 { // fanout, worker_a, worker_b, sync
		t.Fatalf("expected 4 nodes, got %d: %v", len(nodes), nodes)
	}
}

// TestHumanNode verifies the AST can represent a human-in-the-loop node.
func TestHumanNode(t *testing.T) {
	h := &ast.HumanDecl{
		Name:         "checkpoint",
		Input:        "decision_assessment",
		Output:       "human_answers",
		Publish:      "human_decisions",
		Instructions: "clarification_prompt",
		Mode:         ast.HumanPauseUntilAnswers,
		MinAnswers:   1,
	}

	if h.Mode.String() != "pause_until_answers" {
		t.Fatalf("unexpected mode: %s", h.Mode.String())
	}
	if h.MinAnswers != 1 {
		t.Fatal("expected min_answers=1")
	}
}

// TestToolNode verifies the AST can represent a direct tool execution node.
func TestToolNode(t *testing.T) {
	tn := &ast.ToolNodeDecl{
		Name:    "run_ci",
		Command: "${CI_COMMAND}",
		Output:  "ci_run_result",
	}

	if tn.Command != "${CI_COMMAND}" {
		t.Fatalf("unexpected command: %s", tn.Command)
	}
}

// TestBudgetBlock verifies budget fields.
func TestBudgetBlock(t *testing.T) {
	b := &ast.BudgetBlock{
		MaxParallelBranches: 4,
		MaxDuration:         "60m",
		MaxCostUSD:          30.0,
		MaxTokens:           800000,
	}

	if b.MaxParallelBranches != 4 {
		t.Fatalf("expected 4 branches, got %d", b.MaxParallelBranches)
	}
	if b.MaxCostUSD != 30.0 {
		t.Fatalf("expected $30, got %f", b.MaxCostUSD)
	}
}

// TestSchemaEnumConstraint verifies enum constraints on schema fields.
func TestSchemaEnumConstraint(t *testing.T) {
	f := &ast.SchemaField{
		Name:       "confidence",
		Type:       ast.FieldTypeString,
		EnumValues: []string{"low", "medium", "high"},
	}

	if len(f.EnumValues) != 3 {
		t.Fatalf("expected 3 enum values, got %d", len(f.EnumValues))
	}
	if f.EnumValues[1] != "medium" {
		t.Fatalf("expected 'medium', got %s", f.EnumValues[1])
	}
}

// TestVarsWithDefaults verifies variable declarations with default values.
func TestVarsWithDefaults(t *testing.T) {
	vars := &ast.VarsBlock{
		Fields: []*ast.VarField{
			{
				Name: "workspace_dir",
				Type: ast.TypeString,
				Default: &ast.Literal{
					Kind:   ast.LitString,
					Raw:    `"${PROJECT_DIR}"`,
					StrVal: "${PROJECT_DIR}",
				},
			},
			{
				Name:    "review_rules",
				Type:    ast.TypeString,
				Default: nil, // required, no default
			},
		},
	}

	if len(vars.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(vars.Fields))
	}
	if vars.Fields[0].Default == nil {
		t.Fatal("expected default for workspace_dir")
	}
	if vars.Fields[1].Default != nil {
		t.Fatal("expected no default for review_rules")
	}
}

// TestFullWorkflowCoverage verifies the AST covers all V1 primitives
// from the reference fixture pr_refine_dual_model_parallel_compliance.
func TestFullWorkflowCoverage(t *testing.T) {
	// This test ensures every V1 primitive has an AST representation.
	// We build a simplified version of the reference fixture.

	file := &ast.File{
		Vars: &ast.VarsBlock{
			Fields: []*ast.VarField{
				{Name: "workspace_dir", Type: ast.TypeString, Default: &ast.Literal{Kind: ast.LitString, StrVal: "${PROJECT_DIR}"}},
			},
		},
		Prompts: []*ast.PromptDecl{
			{Name: "review_system", Body: "Tu es un reviewer."},
			{Name: "review_user", Body: "Review: {{input.pr_context}}"},
		},
		Schemas: []*ast.SchemaDecl{
			{Name: "review_request", Fields: []*ast.SchemaField{
				{Name: "pr_context", Type: ast.FieldTypeJSON},
				{Name: "review_rules", Type: ast.FieldTypeString},
			}},
			{Name: "review_result", Fields: []*ast.SchemaField{
				{Name: "approved", Type: ast.FieldTypeBool},
				{Name: "confidence", Type: ast.FieldTypeString, EnumValues: []string{"low", "medium", "high"}},
			}},
			{Name: "act_result", Fields: []*ast.SchemaField{
				{Name: "applied", Type: ast.FieldTypeBool},
				{Name: "files_changed", Type: ast.FieldTypeStringArray},
			}},
		},
		Agents: []*ast.AgentDecl{
			{Name: "context_builder", Model: "${CONTEXT_MODEL}", Input: "repo_context_request", Output: "pr_context", Publish: "pr_context", System: "context_builder_system", User: "context_builder_user", Session: ast.SessionFresh, Tools: []string{"git_diff", "read_file"}, ToolMaxSteps: 8},
			{Name: "claude_review", Model: "${CLAUDE_MODEL}", Input: "review_request", Output: "review_result", System: "review_system", User: "review_user", Session: ast.SessionFresh, Tools: []string{"git_diff", "read_file"}, ToolMaxSteps: 10},
			{Name: "act_on_plan", Model: "${ACT_MODEL}", Input: "act_request", Output: "act_result", Publish: "act_report", System: "act_system", User: "act_user", Session: ast.SessionFresh, Tools: []string{"read_file", "write_file", "patch"}, ToolMaxSteps: 20},
		},
		Judges: []*ast.JudgeDecl{
			{Name: "plan_compliance_check", Model: "${JUDGE_MODEL}", Input: "plan_compliance_request", Output: "compliance_verdict", System: "compliance_system", User: "compliance_user", Session: ast.SessionFresh},
			{Name: "final_compliance", Model: "${JUDGE_MODEL}", Input: "final_reviews_bundle", Output: "compliance_verdict", Publish: "final_verdict", System: "final_compliance_system", User: "final_compliance_user", Session: ast.SessionFresh},
		},
		Routers: []*ast.RouterDecl{
			{Name: "review_fanout", Mode: ast.RouterFanOutAll},
		},
		Joins: []*ast.JoinDecl{
			{Name: "reviews_join", Strategy: ast.JoinWaitAll, Require: []string{"claude_review", "gpt_review"}, Output: "reviews_bundle"},
		},
		Humans: []*ast.HumanDecl{
			{Name: "human_checkpoint", Input: "decision_assessment", Output: "human_answers", Publish: "human_decisions", Instructions: "clarification_prompt", Mode: ast.HumanPauseUntilAnswers, MinAnswers: 1},
		},
		Workflows: []*ast.WorkflowDecl{
			{
				Name: "pr_refine",
				Vars: &ast.VarsBlock{
					Fields: []*ast.VarField{
						{Name: "pr_title", Type: ast.TypeString},
						{Name: "base_ref", Type: ast.TypeString, Default: &ast.Literal{Kind: ast.LitString, StrVal: "origin/main"}},
					},
				},
				Entry: "context_builder",
				Budget: &ast.BudgetBlock{
					MaxParallelBranches: 4,
					MaxDuration:         "60m",
					MaxCostUSD:          30.0,
					MaxTokens:           800000,
				},
				Edges: []*ast.Edge{
					// Simple edge
					{From: "context_builder", To: "review_fanout", With: []*ast.WithEntry{
						{Key: "pr_context", Value: "{{outputs.context_builder}}"},
					}},
					// Fan-out edges
					{From: "review_fanout", To: "claude_review", With: []*ast.WithEntry{
						{Key: "pr_context", Value: "{{input.pr_context}}"},
					}},
					// Conditional edge
					{From: "plan_compliance_check", To: "act_on_plan", When: &ast.WhenClause{Condition: "approved"}},
					// Negated condition + loop
					{From: "plan_compliance_check", To: "context_builder",
						When: &ast.WhenClause{Condition: "approved", Negated: true},
						Loop: &ast.LoopClause{Name: "refine_loop", MaxIterations: 6},
					},
					// Terminal
					{From: "final_compliance", To: "done", When: &ast.WhenClause{Condition: "approved"}},
					{From: "final_compliance", To: "context_builder",
						When: &ast.WhenClause{Condition: "approved", Negated: true},
						Loop: &ast.LoopClause{Name: "full_recipe_loop", MaxIterations: 3},
					},
				},
			},
		},
	}

	// Verify all node types present
	nodes := file.AllNodeNames()
	if len(nodes) != 8 { // 3 agents + 2 judges + 1 router + 1 join + 1 human
		t.Fatalf("expected 8 nodes, got %d: %v", len(nodes), nodes)
	}

	// Verify edge variety
	wf := file.Workflows[0]
	var (
		hasSimple    bool
		hasWhen      bool
		hasNegated   bool
		hasLoop      bool
		hasWith      bool
		hasTerminal  bool
	)
	for _, e := range wf.Edges {
		if e.With != nil && len(e.With) > 0 {
			hasWith = true
		}
		if e.When != nil {
			hasWhen = true
			if e.When.Negated {
				hasNegated = true
			}
		}
		if e.Loop != nil {
			hasLoop = true
		}
		if e.When == nil && e.Loop == nil {
			hasSimple = true
		}
		if ast.ReservedTargets[e.To] {
			hasTerminal = true
		}
	}

	checks := map[string]bool{
		"simple edge":    hasSimple,
		"when clause":    hasWhen,
		"negated when":   hasNegated,
		"loop clause":    hasLoop,
		"with block":     hasWith,
		"terminal (done)": hasTerminal,
	}
	for desc, ok := range checks {
		if !ok {
			t.Errorf("missing edge pattern: %s", desc)
		}
	}

	// Verify budget
	if wf.Budget == nil {
		t.Fatal("expected budget block")
	}
	if wf.Budget.MaxCostUSD != 30.0 {
		t.Fatalf("expected $30 budget, got %f", wf.Budget.MaxCostUSD)
	}

	// Verify human node
	if len(file.Humans) != 1 {
		t.Fatal("expected 1 human node")
	}
	if file.Humans[0].Mode != ast.HumanPauseUntilAnswers {
		t.Fatal("expected pause_until_answers mode")
	}

	// Verify publish on agents
	if file.Agents[0].Publish != "pr_context" {
		t.Fatal("expected publish: pr_context on context_builder")
	}
}

package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

func TestUnparseBasic(t *testing.T) {
	f := &ast.File{
		Comments: []*ast.Comment{
			{Text: "Top-level comment"},
		},
		Vars: &ast.VarsBlock{
			Fields: []*ast.VarField{
				{
					Name: "workspace_dir",
					Type: ast.TypeString,
					Default: &ast.Literal{
						Kind:   ast.LitString,
						StrVal: "${PROJECT_DIR}",
					},
				},
				{
					Name: "debug",
					Type: ast.TypeBool,
				},
			},
		},
		Prompts: []*ast.PromptDecl{
			{
				Name: "my_prompt",
				Body: "You are an expert.\nAnalyze the logs.",
			},
		},
		Schemas: []*ast.SchemaDecl{
			{
				Name: "my_schema",
				Fields: []*ast.SchemaField{
					{Name: "branch", Type: ast.FieldTypeString},
					{Name: "confidence", Type: ast.FieldTypeString, EnumValues: []string{"low", "medium", "high"}},
					{Name: "tags", Type: ast.FieldTypeStringArray},
				},
			},
		},
		Agents: []*ast.AgentDecl{
			{
				Name:            "my_agent",
				Model:           "${MODEL}",
				Input:           "my_schema",
				Output:          "result",
				Publish:         "diagnosis",
				System:          "my_prompt",
				User:            "user_prompt",
				Session:         ast.SessionFresh,
				Tools:           []string{"read_file", "list_files"},
				ToolMaxSteps:    10,
				ReasoningEffort: "high",
			},
		},
		Judges: []*ast.JudgeDecl{
			{
				Name:            "my_judge",
				Model:           "${MODEL}",
				Input:           "verify_req",
				Output:          "verdict",
				System:          "judge_sys",
				User:            "judge_user",
				Session:         ast.SessionFresh,
				ReasoningEffort: "low",
			},
		},
		Routers: []*ast.RouterDecl{
			{Name: "my_router", Mode: ast.RouterFanOutAll},
		},
		Humans: []*ast.HumanDecl{
			{
				Name:         "my_human",
				Input:        "review_in",
				Output:       "review_out",
				Interaction:  ast.InteractionHuman,
				Instructions: "review_prompt",
				MinAnswers:   1,
				Model:        "${MODEL}",
				System:       "review_sys",
			},
		},
		Tools: []*ast.ToolNodeDecl{
			{Name: "run_ci", Command: "${CI_COMMAND}", Input: "ci_input", Output: "ci_result"},
		},
		Workflows: []*ast.WorkflowDecl{
			{
				Name: "my_workflow",
				Vars: &ast.VarsBlock{
					Fields: []*ast.VarField{
						{Name: "branch", Type: ast.TypeString},
						{Name: "ci_command", Type: ast.TypeString, Default: &ast.Literal{Kind: ast.LitString, StrVal: "make test"}},
					},
				},
				Entry: "my_agent",
				Budget: &ast.BudgetBlock{
					MaxParallelBranches: 1,
					MaxDuration:         "30m",
					MaxCostUSD:          15,
					MaxTokens:           400000,
					MaxIterations:       10,
				},
				Edges: []*ast.Edge{
					{
						From: "my_agent",
						To:   "plan_fix",
						With: []*ast.WithEntry{
							{Key: "diagnosis", Value: "{{outputs.diagnose}}"},
							{Key: "repo_context", Value: "{{vars.repo_context}}"},
						},
					},
					{
						From: "plan_fix",
						To:   "act_fix",
					},
					{
						From: "verify_ci",
						To:   "done",
						When: &ast.WhenClause{Condition: "green"},
					},
					{
						From: "verify_ci",
						To:   "diagnose",
						When: &ast.WhenClause{Condition: "green", Negated: true},
						Loop: &ast.LoopClause{Name: "fix_loop", MaxIterations: 5},
						With: []*ast.WithEntry{
							{Key: "ci_logs", Value: "{{outputs.run_ci.logs}}"},
							{Key: "previous_attempts", Value: "{{outputs.verify_ci.summary}}"},
						},
					},
				},
			},
		},
	}

	got := Unparse(f)

	// Verify key fragments are present.
	checks := []string{
		"## Top-level comment",
		"vars:\n  workspace_dir: string = \"${PROJECT_DIR}\"\n  debug: bool",
		"prompt my_prompt:\n  You are an expert.\n  Analyze the logs.",
		"confidence: string [enum: \"low\", \"medium\", \"high\"]",
		"tags: string[]",
		"agent my_agent:",
		"model: \"${MODEL}\"",
		"tools: [read_file, list_files]",
		"tool_max_steps: 10",
		"reasoning_effort: high",
		"judge my_judge:",
		"reasoning_effort: low",
		"router my_router:\n  mode: fan_out_all",
		"human my_human:",
		"min_answers: 1",
		"tool run_ci:",
		"command: \"${CI_COMMAND}\"",
		"input: ci_input",
		"output: ci_result",
		"workflow my_workflow:",
		"  vars:\n    branch: string\n    ci_command: string = \"make test\"",
		"  entry: my_agent",
		"  budget:\n    max_parallel_branches: 1\n    max_duration: \"30m\"\n    max_cost_usd: 15\n    max_tokens: 400000\n    max_iterations: 10",
		"my_agent -> plan_fix with {",
		"diagnosis: \"{{outputs.diagnose}}\"",
		"plan_fix -> act_fix",
		"verify_ci -> done when green",
		"verify_ci -> diagnose when not green as fix_loop(5) with {",
	}

	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("output missing expected fragment:\n  want: %q\n\nfull output:\n%s", want, got)
		}
	}
}

func TestUnparseEmpty(t *testing.T) {
	f := &ast.File{}
	got := Unparse(f)
	if got != "" {
		t.Errorf("expected empty output for empty file, got:\n%s", got)
	}
}

func TestUnparseSingleEdgeWith(t *testing.T) {
	f := &ast.File{
		Workflows: []*ast.WorkflowDecl{
			{
				Name:  "w",
				Entry: "a",
				Edges: []*ast.Edge{
					{
						From: "a",
						To:   "b",
						With: []*ast.WithEntry{
							{Key: "x", Value: "val"},
						},
					},
				},
			},
		},
	}
	got := Unparse(f)
	if !strings.Contains(got, "a -> b with {") {
		t.Errorf("single with entry should use multi-line format, got:\n%s", got)
	}
	if !strings.Contains(got, `x: "val"`) {
		t.Errorf("with entry missing, got:\n%s", got)
	}
}

func TestUnparseCriticalEditorFields(t *testing.T) {
	threshold := 0.82
	preserve := 7
	interaction := ast.InteractionLLMOrHuman
	inherit := false
	autoload := true
	f := &ast.File{
		MCPServers: []*ast.MCPServerDecl{{
			Name:      "github",
			Transport: ast.MCPTransportHTTP,
			URL:       "https://api.githubcopilot.com/mcp",
			Auth: &ast.MCPAuthDecl{
				Type:      "oauth2",
				AuthURL:   "https://github.com/login/oauth/authorize",
				TokenURL:  "https://github.com/login/oauth/access_token",
				RevokeURL: "https://github.com/login/oauth/revoke",
				ClientID:  "Iv1.iterion-demo",
				Scopes:    []string{"repo", "read:org"},
			},
		}},
		Agents: []*ast.AgentDecl{{
			Name:       "implement",
			Backend:    "claude_code",
			MCP:        &ast.MCPConfigDecl{Inherit: &inherit, Servers: []string{"github"}, Disable: []string{"local"}},
			Session:    ast.SessionFresh,
			MaxTokens:  2048,
			Readonly:   true,
			Compaction: &ast.CompactionBlock{Threshold: &threshold, PreserveRecent: &preserve},
		}},
		Workflows: []*ast.WorkflowDecl{{
			Name:           "flow",
			Entry:          "implement",
			DefaultBackend: "claude_code",
			MCP:            &ast.MCPConfigDecl{AutoloadProject: &autoload, Servers: []string{"github"}},
			Compaction:     &ast.CompactionBlock{Threshold: &threshold, PreserveRecent: &preserve},
			Interaction:    &interaction,
			Edges:          []*ast.Edge{{From: "implement", To: "done"}},
		}},
	}

	got := Unparse(f)
	checks := []string{
		"mcp_server github:",
		"  auth:\n    type: \"oauth2\"",
		"    scopes: [\"repo\", \"read:org\"]",
		"  mcp:\n    inherit: false\n    servers: [github]\n    disable: [local]",
		"  max_tokens: 2048",
		"  readonly: true",
		"  compaction:\n    threshold: 0.82\n    preserve_recent: 7",
		"  default_backend: \"claude_code\"",
		"  interaction: llm_or_human",
		"  mcp:\n    autoload_project: true\n    servers: [github]",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing expected fragment %q\nfull output:\n%s", want, got)
		}
	}
}

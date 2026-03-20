package model

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	goai "github.com/zendev-sh/goai"

	"github.com/iterion-ai/iterion/ir"
	"github.com/iterion-ai/iterion/tool"
)

// ---------------------------------------------------------------------------
// Helper: build a minimal workflow + executor with a tool registry and policy
// ---------------------------------------------------------------------------

func actExecutor(t *testing.T, policy *tool.Policy, builtins map[string]func(ctx context.Context, input json.RawMessage) (string, error)) *GoaiExecutor {
	t.Helper()

	reg := NewRegistry()
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	tr := tool.NewRegistry()
	for name, exec := range builtins {
		if err := tr.RegisterBuiltin(name, "test tool "+name, nil, exec); err != nil {
			t.Fatalf("register builtin %q: %v", name, err)
		}
	}

	return NewGoaiExecutor(reg, wf,
		WithToolRegistry(tr),
		WithToolPolicy(policy),
	)
}

// noop execute function for tools that return simple JSON.
func jsonExec(result string) func(context.Context, json.RawMessage) (string, error) {
	return func(_ context.Context, _ json.RawMessage) (string, error) {
		return result, nil
	}
}

// ---------------------------------------------------------------------------
// Tool node: policy allows the command
// ---------------------------------------------------------------------------

func TestActToolNodeAllowed(t *testing.T) {
	policy := tool.NewPolicy("run_command", "git_diff")
	exec := actExecutor(t, policy, map[string]func(context.Context, json.RawMessage) (string, error){
		"run_command": jsonExec(`{"exit_code":0,"stdout":"ok"}`),
		"git_diff":    jsonExec(`{"diff":"+ added line"}`),
	})

	// run_command allowed
	node := &ir.Node{ID: "act_cmd", Kind: ir.NodeTool, Command: "run_command"}
	output, err := exec.Execute(context.Background(), node, map[string]interface{}{"cmd": "go test ./..."})
	if err != nil {
		t.Fatalf("expected run_command to be allowed, got: %v", err)
	}
	if output["exit_code"] != float64(0) {
		t.Errorf("expected exit_code 0, got %v", output["exit_code"])
	}

	// git_diff allowed
	node2 := &ir.Node{ID: "act_diff", Kind: ir.NodeTool, Command: "git_diff"}
	output2, err := exec.Execute(context.Background(), node2, nil)
	if err != nil {
		t.Fatalf("expected git_diff to be allowed, got: %v", err)
	}
	if output2["diff"] != "+ added line" {
		t.Errorf("unexpected diff: %v", output2["diff"])
	}
}

// ---------------------------------------------------------------------------
// Tool node: policy denies the command
// ---------------------------------------------------------------------------

func TestActToolNodeDenied(t *testing.T) {
	policy := tool.NewPolicy("git_diff") // only git_diff allowed
	exec := actExecutor(t, policy, map[string]func(context.Context, json.RawMessage) (string, error){
		"run_command": jsonExec(`{"exit_code":0}`),
		"git_diff":    jsonExec(`{"diff":""}`),
	})

	node := &ir.Node{ID: "act_cmd", Kind: ir.NodeTool, Command: "run_command"}
	_, err := exec.Execute(context.Background(), node, nil)
	if err == nil {
		t.Fatal("expected run_command to be denied")
	}
	if !errors.Is(err, tool.ErrToolDenied) {
		t.Errorf("expected ErrToolDenied, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tool node: deny-all policy
// ---------------------------------------------------------------------------

func TestActDenyAllPolicyRejectsEverything(t *testing.T) {
	policy := tool.DenyAllPolicy()
	exec := actExecutor(t, policy, map[string]func(context.Context, json.RawMessage) (string, error){
		"run_command": jsonExec(`{}`),
	})

	node := &ir.Node{ID: "act", Kind: ir.NodeTool, Command: "run_command"}
	_, err := exec.Execute(context.Background(), node, nil)
	if err == nil {
		t.Fatal("deny-all policy should reject")
	}
	if !errors.Is(err, tool.ErrToolDenied) {
		t.Errorf("expected ErrToolDenied, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tool node: nil (open) policy allows everything
// ---------------------------------------------------------------------------

func TestActNilPolicyAllowsEverything(t *testing.T) {
	exec := actExecutor(t, nil, map[string]func(context.Context, json.RawMessage) (string, error){
		"run_command": jsonExec(`{"exit_code":0}`),
	})

	node := &ir.Node{ID: "act", Kind: ir.NodeTool, Command: "run_command"}
	output, err := exec.Execute(context.Background(), node, nil)
	if err != nil {
		t.Fatalf("nil policy should allow, got: %v", err)
	}
	if output["exit_code"] != float64(0) {
		t.Errorf("unexpected output: %v", output)
	}
}

// ---------------------------------------------------------------------------
// Tool node: wildcard policy allows everything
// ---------------------------------------------------------------------------

func TestActWildcardPolicyAllowsEverything(t *testing.T) {
	policy := tool.NewPolicy("*")
	exec := actExecutor(t, policy, map[string]func(context.Context, json.RawMessage) (string, error){
		"run_command": jsonExec(`{"exit_code":0}`),
	})

	node := &ir.Node{ID: "act", Kind: ir.NodeTool, Command: "run_command"}
	_, err := exec.Execute(context.Background(), node, nil)
	if err != nil {
		t.Fatalf("wildcard policy should allow, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tool node: prefix wildcard for MCP namespace
// ---------------------------------------------------------------------------

func TestActPrefixWildcardMCP(t *testing.T) {
	policy := tool.NewPolicy("mcp.github.*")

	reg := NewRegistry()
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	tr := tool.NewRegistry()
	_ = tr.RegisterMCP("github", "create_issue", "create issue", nil, jsonExec(`{"id":42}`))
	_ = tr.RegisterMCP("slack", "post_message", "post message", nil, jsonExec(`{}`))

	exec := NewGoaiExecutor(reg, wf, WithToolRegistry(tr), WithToolPolicy(policy))

	// mcp.github.create_issue → allowed
	node := &ir.Node{ID: "n1", Kind: ir.NodeTool, Command: "mcp.github.create_issue"}
	_, err := exec.Execute(context.Background(), node, nil)
	if err != nil {
		t.Fatalf("mcp.github.create_issue should be allowed: %v", err)
	}

	// mcp.slack.post_message → denied
	node2 := &ir.Node{ID: "n2", Kind: ir.NodeTool, Command: "mcp.slack.post_message"}
	_, err = exec.Execute(context.Background(), node2, nil)
	if err == nil {
		t.Fatal("mcp.slack.post_message should be denied")
	}
	if !errors.Is(err, tool.ErrToolDenied) {
		t.Errorf("expected ErrToolDenied, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Hook fires on denied tool
// ---------------------------------------------------------------------------

func TestActDeniedToolFiresHook(t *testing.T) {
	policy := tool.NewPolicy("git_diff")

	var hookCalled bool
	var hookErr error

	reg := NewRegistry()
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	tr := tool.NewRegistry()
	_ = tr.RegisterBuiltin("run_command", "run cmd", nil, jsonExec(`{}`))

	exec := NewGoaiExecutor(reg, wf,
		WithToolRegistry(tr),
		WithToolPolicy(policy),
		WithEventHooks(EventHooks{
			OnToolCall: func(nodeID string, info goai.ToolCallInfo) {
				hookCalled = true
				hookErr = info.Error
			},
		}),
	)

	node := &ir.Node{ID: "act", Kind: ir.NodeTool, Command: "run_command"}
	_, _ = exec.Execute(context.Background(), node, nil)

	if !hookCalled {
		t.Fatal("OnToolCall hook should have been called on denied tool")
	}
	if !errors.Is(hookErr, tool.ErrToolDenied) {
		t.Errorf("hook error should be ErrToolDenied, got: %v", hookErr)
	}
}

// ---------------------------------------------------------------------------
// Act phase artifacts: command_results, test_results, applied_patch, git_diff_after_act
// ---------------------------------------------------------------------------

func TestActArtifactsProduced(t *testing.T) {
	policy := tool.NewPolicy("run_command", "run_tests", "apply_patch", "git_diff")
	exec := actExecutor(t, policy, map[string]func(context.Context, json.RawMessage) (string, error){
		"run_command": jsonExec(`{"exit_code":0,"stdout":"compiled"}`),
		"run_tests":   jsonExec(`{"passed":true,"total":5,"failed":0}`),
		"apply_patch": jsonExec(`{"applied":true,"files_changed":["main.go"]}`),
		"git_diff":    jsonExec(`{"diff":"diff --git a/main.go b/main.go\n+// fixed"}`),
	})

	cases := []struct {
		nodeID  string
		command string
		key     string
		want    interface{}
	}{
		{"cmd", "run_command", "exit_code", float64(0)},
		{"tests", "run_tests", "passed", true},
		{"patch", "apply_patch", "applied", true},
		{"diff", "git_diff", "diff", "diff --git a/main.go b/main.go\n+// fixed"},
	}

	for _, tc := range cases {
		node := &ir.Node{ID: tc.nodeID, Kind: ir.NodeTool, Command: tc.command}
		output, err := exec.Execute(context.Background(), node, nil)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.nodeID, err)
		}
		if output[tc.key] != tc.want {
			t.Errorf("%s: got %v, want %v", tc.nodeID, output[tc.key], tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Legacy tool path with policy (backward compat)
// ---------------------------------------------------------------------------

func TestActPolicyWithLegacyTools(t *testing.T) {
	policy := tool.NewPolicy("git_diff") // only git_diff

	reg := NewRegistry()
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}

	tools := map[string]goai.Tool{
		"git_diff": {
			Name: "git_diff",
			Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
				return `{"diff":"ok"}`, nil
			},
		},
		"run_command": {
			Name: "run_command",
			Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
				return `{}`, nil
			},
		},
	}

	exec := NewGoaiExecutor(reg, wf,
		WithToolImplementations(tools),
		WithToolPolicy(policy),
	)

	// git_diff → allowed
	node := &ir.Node{ID: "n1", Kind: ir.NodeTool, Command: "git_diff"}
	_, err := exec.Execute(context.Background(), node, nil)
	if err != nil {
		t.Fatalf("git_diff should be allowed: %v", err)
	}

	// run_command → denied
	node2 := &ir.Node{ID: "n2", Kind: ir.NodeTool, Command: "run_command"}
	_, err = exec.Execute(context.Background(), node2, nil)
	if err == nil {
		t.Fatal("run_command should be denied")
	}
	if !errors.Is(err, tool.ErrToolDenied) {
		t.Errorf("expected ErrToolDenied, got: %v", err)
	}
}

package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/tool"
)

func TestExecuteToolNodeRejectsInactiveMCPServer(t *testing.T) {
	reg := NewRegistry()
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	tr := tool.NewRegistry()
	if err := tr.RegisterMCP("github", "create_issue", "GitHub issue", nil, jsonExec(`{"ok":true}`)); err != nil {
		t.Fatalf("register github MCP tool: %v", err)
	}
	if err := tr.RegisterMCP("slack", "post_message", "Slack post", nil, jsonExec(`{"ok":true}`)); err != nil {
		t.Fatalf("register slack MCP tool: %v", err)
	}

	exec := NewGoaiExecutor(reg, wf, WithToolRegistry(tr))

	allowed := &ir.Node{
		ID:               "n1",
		Kind:             ir.NodeTool,
		Command:          "mcp.github.create_issue",
		ActiveMCPServers: []string{"github"},
	}
	if _, err := exec.Execute(context.Background(), allowed, nil); err != nil {
		t.Fatalf("github MCP tool should be allowed: %v", err)
	}

	denied := &ir.Node{
		ID:               "n2",
		Kind:             ir.NodeTool,
		Command:          "mcp.slack.post_message",
		ActiveMCPServers: []string{"github"},
	}
	if _, err := exec.Execute(context.Background(), denied, nil); err == nil {
		t.Fatal("expected inactive MCP server to be rejected")
	}
}

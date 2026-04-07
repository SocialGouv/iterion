package model

import (
	"testing"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/tool"
)

func TestCheckNodeToolAccessRejectsInactiveMCPServer(t *testing.T) {
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

	// AgentNode with only "github" active — github tools allowed, slack denied.
	allowed := &ir.AgentNode{
		BaseNode:         ir.BaseNode{ID: "n1"},
		ActiveMCPServers: []string{"github"},
	}
	if err := exec.checkNodeToolAccess(allowed, "mcp.github.create_issue"); err != nil {
		t.Fatalf("github MCP tool should be allowed: %v", err)
	}

	denied := &ir.AgentNode{
		BaseNode:         ir.BaseNode{ID: "n2"},
		ActiveMCPServers: []string{"github"},
	}
	if err := exec.checkNodeToolAccess(denied, "mcp.slack.post_message"); err == nil {
		t.Fatal("expected inactive MCP server to be rejected")
	}

	// ToolNode has no ActiveMCPServers — all MCP tools allowed (no restriction).
	toolNode := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "n3"},
		Command:  "mcp.slack.post_message",
	}
	if err := exec.checkNodeToolAccess(toolNode, "mcp.slack.post_message"); err != nil {
		t.Fatalf("ToolNode without ActiveMCPServers should allow all MCP tools: %v", err)
	}
}

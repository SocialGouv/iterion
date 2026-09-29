package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Almost every MCP tool is registered per server (`mcp.<server>.<tool>`), so
// the node's `mcp:` declaration scopes it by construction. Three do not:
// list_mcp_resources, read_mcp_resource and mcp_auth take the server's NAME
// as an argument the MODEL writes, and they reach the launcher's MCP
// provider, which connects the named server — for a stdio server, starts its
// process.
//
// So the node's declaration has to be enforced on the argument. The three
// states are distinguished on purpose: checkNodeToolAccess reads an EMPTY
// active set as "unrestricted", which is right for a tool node and says the
// opposite of what an agent with `inherit: false` wrote.
func TestTheServerNamingMCPToolsHonourTheNodesScope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		active  []string
		server  string
		allowed bool
	}{
		{"a server the node declared", []string{"allowed", "other"}, "allowed", true},
		{"a server another node declared", []string{"allowed"}, "other", false},
		{"no MCP scope at all denies every server", nil, "allowed", false},
		{"the implicit default server", []string{"allowed"}, "", false},
	} {
		for _, toolName := range []string{"list_mcp_resources", "read_mcp_resource", "mcp_auth"} {
			t.Run(tc.name+"/"+toolName, func(t *testing.T) {
				var reached string
				e := scopingExecutor(t, toolName, func(input json.RawMessage) {
					reached = string(input)
				})
				node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n"}, ActiveMCPServers: tc.active}
				defs, _, err := e.resolveToolsForNode(context.Background(), node, []string{toolName})
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				if len(defs) != 1 {
					t.Fatalf("expected the tool to resolve, got %d definitions", len(defs))
				}

				input := json.RawMessage(`{}`)
				if tc.server != "" {
					input = json.RawMessage(`{"server":"` + tc.server + `"}`)
				}
				_, err = defs[0].Execute(context.Background(), input)

				if tc.allowed {
					if err != nil {
						t.Fatalf("a server the node declared must be reachable: %v", err)
					}
					if reached == "" {
						t.Error("the underlying tool was never called")
					}
					return
				}
				if err == nil {
					t.Fatal("a server outside the node's scope must be refused")
				}
				if reached != "" {
					t.Errorf("the refusal must happen BEFORE the provider is reached (it connects the server): %q", reached)
				}
				if !strings.Contains(err.Error(), "MCP server") {
					t.Errorf("the refusal should name what was refused: %v", err)
				}
			})
		}
	}
}

// A tool node has no MCP scope to speak of — it is not an LLM turn, nothing
// picks a server name from a model — and wrapping it would refuse a call the
// author wrote explicitly.
func TestANonLLMNodeKeepsTheServerNamingToolsUnwrapped(t *testing.T) {
	var reached bool
	e := scopingExecutor(t, "list_mcp_resources", func(json.RawMessage) { reached = true })
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}}

	def := e.scopeMCPServerNamingTool(delegate.ToolDef{
		Name: "list_mcp_resources",
		Execute: func(_ context.Context, in json.RawMessage) (string, error) {
			reached = true
			return "ok", nil
		},
	}, node)
	if _, err := def.Execute(context.Background(), []byte(`{"server":"anything"}`)); err != nil {
		t.Fatalf("a tool node must not be scoped here: %v", err)
	}
	if !reached {
		t.Error("the call never reached the tool")
	}
}

func scopingExecutor(t *testing.T, toolName string, onCall func(json.RawMessage)) *ClawExecutor {
	t.Helper()
	tr := tool.NewRegistry()
	if err := tr.RegisterBuiltin(toolName, toolName, nil, func(_ context.Context, in json.RawMessage) (string, error) {
		onCall(in)
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	return &ClawExecutor{logger: iterlog.Nop(), toolRegistry: tr}
}

package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// A workflow's `mcp_server:` blocks have to reach EVERY backend that consumes
// them. Gating the hand-off on claude_code alone left pi's whole MCP bridge
// inert in production — and silently, because a node that simply has no tools
// looks like a node that chose not to use them. Every test that exercised the
// bridge hand-built a delegate.Task, so none of them crossed this seam.
func TestBuildTaskForwardsDeclaredMCPServersPerBackend(t *testing.T) {
	e := &ClawExecutor{
		logger: iterlog.Nop(),
		mcpManager: mcp.NewManager(map[string]*mcp.ServerConfig{
			"probe": {Command: "probe-server", Args: []string{"--stdio"}},
		}),
	}
	node := &ir.AgentNode{}
	node.ID = "n"
	f := backendFields{id: "n", activeMCPServers: []string{"probe"}}

	// Both CLI backends that consume the list must receive it. pi is the
	// regression: it went through the real executor with an empty list.
	for _, backend := range []string{delegate.BackendClaudeCode, delegate.BackendPi} {
		t.Run(backend, func(t *testing.T) {
			task, err := e.buildTask(context.Background(), node, f, map[string]any{}, backend, nil)
			if err != nil {
				t.Fatalf("buildTask: %v", err)
			}
			if len(task.MCPServers) != 1 {
				t.Fatalf("declared MCP server not forwarded to %s: %+v", backend, task.MCPServers)
			}
			if task.MCPServers[0].Command != "probe-server" {
				t.Errorf("server config not resolved for %s: %+v", backend, task.MCPServers[0])
			}
		})
	}

	// The exclusions are a deliberate policy, not an oversight: claw resolves
	// the same servers in-process into ToolDefs, and kimi/grok run a tool set
	// iterion does not configure. Asserted on the set rather than through
	// buildTask, which needs far more executor wiring for those paths.
	for _, backend := range []string{delegate.BackendClaw, delegate.BackendKimi, delegate.BackendGrok} {
		if mcpForwardingBackends[backend] {
			t.Errorf("%s must not receive Task.MCPServers", backend)
		}
	}
}

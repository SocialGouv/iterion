package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
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

// An AMBIENT MCP server (repo .mcp.json / plugin catalog — never named by
// the node) that cannot boot must cost its own tools, never the run: the
// other backends already degrade per-server, and hard-failing the claw
// splice let one token-less repo server kill every claw node of a run
// (native:b2e46831 — the repo-scoped sentry server on runner pods).
func TestBuildTaskAmbientMCPServerBootFailureDegradesNotFails(t *testing.T) {
	tr := tool.NewRegistry()
	for _, name := range []string{"bash", "todo_write"} {
		if err := tr.RegisterBuiltin(name, name, nil, func(context.Context, json.RawMessage) (string, error) {
			return "ok", nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	e := &ClawExecutor{
		logger:       iterlog.Nop(),
		toolRegistry: tr,
		mcpManager: mcp.NewManager(map[string]*mcp.ServerConfig{
			"deadsrv": {Command: "/nonexistent/iterion-test-mcp-server"},
		}),
	}
	var degraded []MCPServerDegradedInfo
	e.hooks.OnMCPServerDegraded = func(_ string, info MCPServerDegradedInfo) {
		degraded = append(degraded, info)
	}

	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n"}, ActiveMCPServers: []string{"deadsrv"}}
	f := backendFields{id: "n", model: "anthropic/claude-opus-5", tools: []string{"bash"}, activeMCPServers: []string{"deadsrv"}}

	task, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, nil)
	if err != nil {
		t.Fatalf("an ambient server that cannot boot must not fail the node: %v", err)
	}
	names := make([]string, 0, len(task.ToolDefs))
	for _, td := range task.ToolDefs {
		names = append(names, td.Name)
	}
	if len(task.ToolDefs) == 0 {
		t.Fatal("the node's own tools must survive the degraded server")
	}
	for _, n := range names {
		if strings.HasPrefix(n, "mcp.deadsrv.") {
			t.Fatalf("dead server's tools leaked into the task: %v", names)
		}
	}
	if len(degraded) != 1 || degraded[0].Server != "deadsrv" || degraded[0].Source != "ambient" || degraded[0].Err == nil {
		t.Fatalf("the drop must be observable via OnMCPServerDegraded: %+v", degraded)
	}
}

// The other half of the same rule, and the reason the ambient degrade is safe:
// a server the node names EXPLICITLY is a declared dependency, so its boot
// failure fails the node loud. Without this test the asymmetry reads as an
// accident of which branch returns, and the tolerant half would erode onto the
// declared path — silently running an agent without the tools it asked for.
func TestExplicitMCPWildcardBootFailureFailsTheNode(t *testing.T) {
	tr := tool.NewRegistry()
	if err := tr.RegisterBuiltin("bash", "bash", nil, func(context.Context, json.RawMessage) (string, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	e := &ClawExecutor{
		logger:       iterlog.Nop(),
		toolRegistry: tr,
		mcpManager: mcp.NewManager(map[string]*mcp.ServerConfig{
			"deadsrv": {Command: "/nonexistent/iterion-test-mcp-server"},
		}),
	}
	var degraded []MCPServerDegradedInfo
	e.hooks.OnMCPServerDegraded = func(_ string, info MCPServerDegradedInfo) {
		degraded = append(degraded, info)
	}

	// Named in the node's own tool list, NOT in activeMCPServers — the whole
	// distinction between a declared dependency and an inherited one.
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n"}}
	f := backendFields{id: "n", model: "anthropic/claude-opus-5", tools: []string{"bash", "mcp.deadsrv.*"}}

	_, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, nil)
	if err == nil {
		t.Fatal("an explicitly named MCP server that cannot boot must fail the node")
	}
	// The message has to name the server and say the node asked for it, or the
	// operator cannot tell this apart from a tool that simply does not exist.
	if !strings.Contains(err.Error(), "deadsrv") || !strings.Contains(err.Error(), "explicitly") {
		t.Errorf("error must name the server and its declared origin: %v", err)
	}
	if len(degraded) != 0 {
		t.Errorf("a declared dependency must never be reported as a degrade: %+v", degraded)
	}
}

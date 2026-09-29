package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// refusingExecutor builds an executor whose MCP catalog holds one
// workflow-controlled server and whose start policy is the sandboxed one, so
// every attempt to start that server is refused rather than attempted.
func refusingExecutor(t *testing.T, extraTools ...string) *ClawExecutor {
	t.Helper()
	tr := tool.NewRegistry()
	for _, name := range append([]string{"bash", "todo_write"}, extraTools...) {
		if err := tr.RegisterBuiltin(name, name, nil, func(context.Context, json.RawMessage) (string, error) {
			return "ok", nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return &ClawExecutor{
		logger:       iterlog.Nop(),
		toolRegistry: tr,
		mcpManager: mcp.NewManager(map[string]*mcp.ServerConfig{
			"repo": {
				Name: "repo", Origin: mcp.OriginProject,
				Transport: mcp.TransportStdio, Command: "/bin/echo",
			},
		}, mcp.WithStartPolicy(mcp.StartOperatorServersOnly)),
	}
}

// A server the launcher may not start is not a broken server, and the two
// must not share an outcome. A boot failure fails the node at build time; a
// refusal has to survive as far as EXECUTION, because a build error aborts
// the node before its `fallbacks:` are walked — and the whole point of the
// refusal is that another backend, which starts that server inside the
// container, can serve this node.
//
// Both spellings are covered: the wildcard the ambient splice and `tools:`
// use, and an exact `mcp.<server>.<tool>`. The exact one is the trap — its
// tool is never registered (discovery never ran), so anything short of
// skipping resolution for the whole server surfaces as "unknown tool" at
// build time, which names neither the server nor the reason.
func TestARefusedMCPServerReachesExecuteRatherThanFailingTheBuild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools []string
	}{
		{"wildcard", []string{"bash", "mcp.repo.*"}},
		{"exact name", []string{"bash", "mcp.repo.search"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := refusingExecutor(t)
			node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n"}, ActiveMCPServers: []string{"repo"}}
			f := backendFields{
				id: "n", model: "anthropic/claude-opus-5",
				tools: tc.tools, activeMCPServers: []string{"repo"},
			}

			task, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, nil)
			if err != nil {
				t.Fatalf("a refusal must not fail the build — the node's fallbacks would never run: %v", err)
			}
			if len(task.MCPServersRefusedOnLauncher) != 1 {
				t.Fatalf("the refusal must travel on the task, got %v", task.MCPServersRefusedOnLauncher)
			}
			reason, ok := task.MCPServersRefusedOnLauncher["repo"]
			if !ok {
				t.Fatalf("the refusal must name the server: %v", task.MCPServersRefusedOnLauncher)
			}
			if !strings.Contains(reason, "project") {
				t.Errorf("the reason must name the origin, so the operator can see WHY: %q", reason)
			}
			for _, td := range task.ToolDefs {
				if strings.HasPrefix(td.Name, "mcp_repo") || strings.HasPrefix(td.Name, "mcp.repo") {
					t.Errorf("a refused server's tools must not be advertised: %q", td.Name)
				}
			}
		})
	}
}

// And at execution, the sandboxed claw route refuses BY TYPE, which is what
// makes the fallback chain walk on to a route that can serve the node.
func TestSandboxedClawRefusesARefusedMCPServerByCapability(t *testing.T) {
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	task := delegate.Task{
		NodeID:  "n",
		Sandbox: unmetSandboxRun{},
		MCPServersRefusedOnLauncher: map[string]string{
			"repo":  "mcp: server \"repo\" (origin: project) is not started by the launcher",
			"other": "mcp: server \"other\" (origin: workflow) is not started by the launcher",
		},
	}

	_, err := b.Execute(context.Background(), task)
	var unsupported *delegate.ErrCapabilityUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected a typed capability refusal so the chain continues, got %v", err)
	}
	// Both names, in a stable order: the capability string is what a fallback
	// decision is logged under, and one that reshuffles per run makes two
	// identical runs look different.
	if !strings.Contains(unsupported.Capability, `"other", "repo"`) {
		t.Errorf("the capability must name every refused server in a stable order: %q", unsupported.Capability)
	}
}

// An AMBIENT server — one the node never named, inherited from the target
// repository or the plugin catalog — degrades instead: the node runs without
// its tools, loudly. Failing the node there would make one repository's
// `.mcp.json` able to stop every sandboxed claw node of every run.
func TestAnAmbientRefusedServerDegradesTheNode(t *testing.T) {
	e := refusingExecutor(t)
	var degraded []MCPServerDegradedInfo
	e.hooks.OnMCPServerDegraded = func(_ string, info MCPServerDegradedInfo) {
		degraded = append(degraded, info)
	}

	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n"}, ActiveMCPServers: []string{"repo"}}
	f := backendFields{
		id: "n", model: "anthropic/claude-opus-5",
		tools: []string{"bash"}, activeMCPServers: []string{"repo"},
	}

	task, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, nil)
	if err != nil {
		t.Fatalf("an ambient refusal must not fail the node: %v", err)
	}
	if len(task.ToolDefs) == 0 {
		t.Fatal("the node's own tools must survive")
	}
	if len(degraded) != 1 || degraded[0].Server != "repo" || degraded[0].Err == nil {
		t.Fatalf("the drop must be observable via OnMCPServerDegraded: %+v", degraded)
	}
	if !strings.Contains(degraded[0].Err.Error(), "project") {
		t.Errorf("the degrade reason must name the origin: %v", degraded[0].Err)
	}
	// An ambient server is not a declared dependency, so it does not refuse
	// the node at execution either.
	if len(task.MCPServersRefusedOnLauncher) != 0 {
		t.Errorf("an ambient server must not turn into an execution-time refusal: %v",
			task.MCPServersRefusedOnLauncher)
	}
}

// A wildcard for a server the node cannot USE must not START that server.
// The access check runs on the expanded tool names — after the server booted
// — so a node that names `mcp.other.*` without `other` in its own `mcp:` set
// would spawn a process beside the launcher and then refuse every tool it
// offers. Least privilege reads the declaration first.
func TestAWildcardDoesNotStartAServerTheNodeCannotUse(t *testing.T) {
	started := false
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
			"other": {
				Name: "other", Origin: mcp.OriginPlugin,
				Transport: mcp.TransportStdio,
				// Any attempt to start it is observable as a boot failure.
				Command: "/nonexistent/iterion-test-mcp-server",
			},
		}, mcp.WithStartPolicy(mcp.StartAllServers)),
	}
	e.hooks.OnMCPServerDegraded = func(string, MCPServerDegradedInfo) { started = true }

	// The node's own MCP scope does not include "other".
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n"}, ActiveMCPServers: []string{"allowed"}}
	defs, refused, err := e.resolveToolsForNode(context.Background(), node, []string{"bash", "mcp.other.*"})
	if err != nil {
		t.Fatalf("an out-of-scope wildcard must not fail the node: %v", err)
	}
	if started {
		t.Error("the server was started for a node that cannot reach its tools")
	}
	if len(refused) != 0 {
		t.Errorf("nothing was refused by policy here — the node simply cannot use it: %v", refused)
	}
	if len(defs) != 1 || defs[0].Name != "bash" {
		t.Errorf("the node's own tools must survive: %+v", defs)
	}
}

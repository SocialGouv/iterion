package model

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ---------------------------------------------------------------------------
// Tool resolution helpers
// ---------------------------------------------------------------------------

// nodeActiveMCPServers delegates to ir.NodeActiveMCPServers.
var nodeActiveMCPServers = ir.NodeActiveMCPServers

// resolveToolsForNode resolves a list of tool names to delegate.ToolDef
// instances for a specific node, ensuring that only tools from the node's
// active MCP servers are exposed. Wildcard entries like "mcp.<server>.*"
// are expanded to all tools discovered from that server.
func (e *ClawExecutor) resolveToolsForNode(ctx context.Context, node ir.Node, names []string) ([]delegate.ToolDef, error) {
	// Expand wildcards (e.g. mcp.claude_code.*) into concrete tool names.
	expanded, err := e.expandWildcards(ctx, node, names)
	if err != nil {
		return nil, err
	}

	if err := e.ensureMCPServers(ctx, node, expanded); err != nil {
		return nil, err
	}

	var tools []delegate.ToolDef
	for _, name := range expanded {
		t, ok, err := e.resolveSingleToolForNode(ctx, node, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if e.toolPolicy != nil {
			t = e.guardTool(t, node)
		}
		tools = append(tools, t)
	}
	return tools, nil
}

// resolveTaskMCPServers projects the node's active MCP server names into
// the transport-agnostic delegate.TaskMCPServer shape that CLI backends
// (claude_code) forward to the agent CLI. Unknown names (not in the MCP
// manager's resolved catalog) are skipped — the manager holds only
// user/plugin servers, so internal ones (ask_user, board) are naturally
// absent. Auth-bearing http/sse servers are forwarded by URL+Headers; the
// OAuth broker path (claw-only in-process resolution) is not replicated
// here, so a server needing dynamic bearer refresh should carry a static
// header instead.
func (e *ClawExecutor) resolveTaskMCPServers(names []string) []delegate.TaskMCPServer {
	if e.mcpManager == nil {
		return nil
	}
	var out []delegate.TaskMCPServer
	for _, name := range names {
		cfg, ok := e.mcpManager.ServerConfig(name)
		if !ok || cfg == nil {
			continue
		}
		out = append(out, delegate.TaskMCPServer{
			Name:      cfg.Name,
			Transport: string(cfg.Transport),
			Command:   cfg.Command,
			Args:      append([]string(nil), cfg.Args...),
			URL:       cfg.URL,
			Headers:   cfg.Headers,
			Env:       cfg.Env,
		})
	}
	return out
}

// expandWildcards replaces wildcard entries ("mcp.<server>.*") with the
// concrete tool names discovered from that MCP server.
func (e *ClawExecutor) expandWildcards(ctx context.Context, node ir.Node, names []string) ([]string, error) {
	var expanded []string
	for _, name := range names {
		if !tool.IsMCPWildcard(name) {
			expanded = append(expanded, name)
			continue
		}
		server, err := tool.ParseMCPWildcard(name)
		if err != nil {
			return nil, fmt.Errorf("model: invalid wildcard %q: %w", name, err)
		}
		// Ensure the server is connected so its tools are in the registry.
		//
		// A wildcard that FAILS here is a declared dependency, so the boot
		// failure is fatal on purpose. Ambient servers (target repo
		// `.mcp.json`, plugin catalog) DO reach this loop as wildcards —
		// buildTask splices each one in as `mcp.<srv>.*` — but only after
		// ensuring it one by one and dropping the ones that cannot boot with
		// an `mcp_server_degraded` event, so every ambient wildcard that gets
		// here is already `discovered` and the EnsureServers below is a no-op
		// for it. Keep that ordering: splice an unensured ambient server in
		// and its boot failure is fatal again. The asymmetry with the
		// empty-match warning below is the point — unreachable means the node
		// asked for something the host cannot supply, empty means the server
		// booted and has nothing to offer.
		if e.mcpManager != nil && e.toolRegistry != nil {
			if err := e.mcpManager.EnsureServers(ctx, e.toolRegistry, []string{server}); err != nil {
				// State the RULE, not the instance: at this point the code
				// cannot tell a node-declared server from an ambient one that
				// was already ensured, so naming the provenance would assert
				// something it does not know.
				return nil, fmt.Errorf("model: MCP server %q, required by this node as %q, cannot boot: %w "+
					"(a server the node names explicitly is a declared dependency and fails the node; the same "+
					"server inherited from the target repo's .mcp.json or the plugin catalog degrades to a "+
					"warning instead)", server, name, err)
			}
		}
		if e.toolRegistry == nil {
			return nil, fmt.Errorf("model: wildcard %q requires a tool registry", name)
		}
		serverTools := e.toolRegistry.ListByServer(server)
		if len(serverTools) == 0 {
			e.logger.Warn("wildcard %q matched no tools (server %q may not be started or has no tools)", name, server)
		}
		for _, td := range serverTools {
			expanded = append(expanded, td.QualifiedName)
		}
	}
	return expanded, nil
}

// resolveSingleToolForNode resolves one tool name in the context of a node.
func (e *ClawExecutor) resolveSingleToolForNode(ctx context.Context, node ir.Node, name string) (delegate.ToolDef, bool, error) {
	if err := e.ensureMCPServers(ctx, node, []string{name}); err != nil {
		return delegate.ToolDef{}, false, err
	}

	if e.toolRegistry == nil {
		return delegate.ToolDef{}, false, fmt.Errorf("no tool registry configured")
	}

	td, err := e.toolRegistry.Resolve(name)
	if err != nil {
		return delegate.ToolDef{}, false, err
	}
	if err := e.checkNodeToolAccess(node, td.QualifiedName); err != nil {
		return delegate.ToolDef{}, false, err
	}
	return td.ToDelegateDef(), true, nil
}

func (e *ClawExecutor) ensureMCPServers(ctx context.Context, node ir.Node, names []string) error {
	if e.mcpManager == nil || e.toolRegistry == nil {
		return nil
	}
	servers := activeMCPServersForNames(node, names)
	if len(servers) == 0 {
		return nil
	}
	return e.mcpManager.EnsureServers(ctx, e.toolRegistry, servers)
}

func activeMCPServersForNames(node ir.Node, names []string) []string {
	mcpServers := nodeActiveMCPServers(node)
	if node == nil || len(mcpServers) == 0 {
		return nil
	}
	active := make(map[string]struct{}, len(mcpServers))
	for _, server := range mcpServers {
		active[server] = struct{}{}
	}

	seen := make(map[string]struct{})
	var servers []string
	for _, name := range names {
		var server string
		// Support wildcard patterns like "mcp.claude_code.*".
		if tool.IsMCPWildcard(name) {
			s, err := tool.ParseMCPWildcard(name)
			if err != nil {
				continue
			}
			server = s
		} else {
			s, _, err := tool.ParseMCPName(name)
			if err != nil {
				continue
			}
			server = s
		}
		if _, ok := active[server]; !ok {
			continue
		}
		if _, ok := seen[server]; ok {
			continue
		}
		seen[server] = struct{}{}
		servers = append(servers, server)
	}
	return servers
}

func (e *ClawExecutor) checkNodeToolAccess(node ir.Node, qualified string) error {
	server, _, err := tool.ParseMCPName(qualified)
	if err != nil {
		return nil
	}
	if node == nil {
		return fmt.Errorf("model: MCP tool %q requires a node context", qualified)
	}
	mcpServers := nodeActiveMCPServers(node)
	if len(mcpServers) == 0 {
		return nil
	}
	for _, active := range mcpServers {
		if active == server {
			return nil
		}
	}
	return fmt.Errorf("model: node %q cannot access MCP tool %q because server %q is not active", node.NodeID(), qualified, server)
}

// ---------------------------------------------------------------------------
// Policy guard
// ---------------------------------------------------------------------------

// guardTool wraps a tool's Execute function with a policy check.
// If the tool is denied, Execute returns an ErrToolDenied error without
// invoking the underlying implementation.
func (e *ClawExecutor) guardTool(t delegate.ToolDef, node ir.Node) delegate.ToolDef {
	original := t.Execute
	name := t.Name
	policy := e.toolPolicy
	nodeID := node.NodeID()
	nodeKind := node.NodeKind().String()
	vars := e.vars
	t.Execute = func(ctx context.Context, input json.RawMessage) (string, error) {
		pctx := tool.PolicyContext{
			Ctx:      ctx,
			NodeID:   nodeID,
			NodeKind: nodeKind,
			ToolName: name,
			Input:    input,
			Vars:     vars,
		}
		if err := policy.CheckContext(pctx); err != nil {
			return "", err
		}
		return original(ctx, input)
	}
	return t
}

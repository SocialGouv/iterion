package model

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/bundle"
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
func (e *ClawExecutor) resolveToolsForNode(ctx context.Context, node ir.Node, names []string) ([]delegate.ToolDef, map[string]string, error) {
	// Expand wildcards (e.g. mcp.claude_code.*) into concrete tool names.
	expanded, refused, err := e.expandWildcards(ctx, node, names)
	if err != nil {
		return nil, nil, err
	}

	// Servers this launcher may not start (the run is sandboxed and their
	// definition is not the operator's) are collected, not raised: the node
	// is refused later, when it EXECUTES, so its `fallbacks:` are walked and
	// a backend that starts the server inside the container can serve it. A
	// boot failure — a server that is allowed here and broken — stays fatal.
	if err := e.collectRefusedMCPServers(ctx, node, expanded, refused); err != nil {
		return nil, nil, err
	}

	var tools []delegate.ToolDef
	var unclassified []string
	seen := make(map[string]string, len(expanded))
	for _, name := range expanded {
		// A tool of a refused server is not resolved at all: discovery never
		// ran, so the registry has no definition for it and resolution would
		// fail as "unknown tool" — an error that names neither the server nor
		// the reason, and that kills the node at build time, before any
		// fallback.
		if refusedMCPServerFor(name, refused) {
			continue
		}
		definition, ok, err := e.resolveSingleToolForNode(ctx, node, name)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue
		}
		t := definition.ToDelegateDef()
		if prior, exists := seen[t.Name]; exists {
			if prior != definition.QualifiedName {
				return nil, nil, fmt.Errorf("model: tools %q and %q have the same model tool name %q", prior, definition.QualifiedName, t.Name)
			}
			continue
		}
		seen[t.Name] = definition.QualifiedName
		if e.toolPolicy != nil {
			// For a sandboxed node, a tool that is not launcher-placed never
			// reaches the guard below: the runner executes it in the container,
			// or the backend refuses the node for it. The policy's verdict is
			// taken now, on the name, and a tool it denies is not advertised —
			// so a denied tool can neither run past the guard nor refuse a node
			// that would never have called it successfully.
			if e.sandbox != nil {
				if placement, _ := tool.SandboxPlacementOf(t.Name); placement != tool.PlacementLauncher {
					pctx := e.toolPolicyContext(ctx, ctx, node, t.Name, definition.QualifiedName, nil)
					pctx.Deterministic = true
					if err := e.toolPolicy.CheckContext(pctx); err != nil {
						if e.logger != nil {
							e.logger.Info("[%s] tool %q withheld from the sandboxed node: %v", node.NodeID(), t.Name, err)
						}
						continue
					}
					if placement == tool.PlacementSandbox {
						unclassified = append(unclassified, t.Name)
					}
				}
			}
			t = e.guardTool(ctx, t, node, definition.QualifiedName)
		}
		// Outside the policy branch: the node's MCP scope is the author's
		// declaration, not a permission rule, so it holds whether or not a
		// tool_policy is configured. The wrapper travels with the ToolDef, so
		// it applies in-process and on the launcher's side of a sandboxed
		// run, which executes these same definitions.
		if e.withholdUnscopedMCPServerNamingTool(t, node) {
			continue
		}
		t = e.scopeMCPServerNamingTool(t, node)
		tools = append(tools, t)
	}
	if mc, ok := e.toolPolicy.(tool.ModelConsultingChecker); ok && mc.ConsultsModel() && len(unclassified) > 0 && e.logger != nil {
		e.logger.Warn("[%s] the LLM tool classifier does not see the calls a sandboxed runner executes in-container %v: "+
			"only a tool_policy allowlist, if one is declared, applies to them", node.NodeID(), unclassified)
	}
	return tools, refused, nil
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
func (e *ClawExecutor) expandWildcards(ctx context.Context, node ir.Node, names []string) ([]string, map[string]string, error) {
	var expanded []string
	refused := map[string]string{}
	for _, name := range names {
		if !tool.IsMCPWildcard(name) {
			expanded = append(expanded, name)
			continue
		}
		server, err := tool.ParseMCPWildcard(name)
		if err != nil {
			return nil, nil, fmt.Errorf("model: invalid wildcard %q: %w", name, err)
		}
		// Least privilege: a wildcard for a server this node cannot use must
		// not START that server. checkNodeToolAccess refuses its tools a few
		// lines below, which is too late — the process is already running
		// beside the launcher by then.
		if !mcpServerActiveForNode(node, server) {
			e.logger.Warn("wildcard %q names MCP server %q, which is not active for node %q — not started", name, server, node.NodeID())
			continue
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
				// A server the launcher must not start is not a boot failure:
				// record it and carry on, so the refusal reaches Execute and
				// the node's fallbacks get their turn.
				if mcp.ServerNotStartable(err) {
					refused[server] = err.Error()
					continue
				}
				// State the RULE, not the instance: at this point the code
				// cannot tell a node-declared server from an ambient one that
				// was already ensured, so naming the provenance would assert
				// something it does not know.
				return nil, nil, fmt.Errorf("model: MCP server %q, required by this node as %q, cannot boot: %w "+
					"(a server the node names explicitly is a declared dependency and fails the node; the same "+
					"server inherited from the target repo's .mcp.json or the plugin catalog degrades to a "+
					"warning instead)", server, name, err)
			}
		}
		if e.toolRegistry == nil {
			return nil, nil, fmt.Errorf("model: wildcard %q requires a tool registry", name)
		}
		serverTools := e.toolRegistry.ListByServer(server)
		if len(serverTools) == 0 {
			e.logger.Warn("wildcard %q matched no tools (server %q may not be started or has no tools)", name, server)
		}
		for _, td := range serverTools {
			expanded = append(expanded, td.QualifiedName)
		}
	}
	return expanded, refused, nil
}

// resolveSingleToolForNode resolves one tool name in the context of a node.
func (e *ClawExecutor) resolveSingleToolForNode(ctx context.Context, node ir.Node, name string) (*tool.ToolDef, bool, error) {
	if err := e.ensureMCPServers(ctx, node, []string{name}); err != nil {
		return nil, false, err
	}

	if e.toolRegistry == nil {
		return nil, false, fmt.Errorf("no tool registry configured")
	}

	td, err := e.resolveToolReference(ctx, name)
	if err != nil {
		return nil, false, err
	}
	if err := e.checkNodeToolAccess(node, td.QualifiedName); err != nil {
		return nil, false, err
	}
	return td, true, nil
}

func (e *ClawExecutor) resolveToolReference(ctx context.Context, name string) (*tool.ToolDef, error) {
	if tool.BuiltinAliasesEnabled(ctx) {
		return e.toolRegistry.ResolveWithAliases(name)
	}
	td, err := e.toolRegistry.Resolve(name)
	if err != nil && toolcatalog.BuiltinAlias(name) != "" {
		// Only add the opt-in remedy when the alias actually resolves. In particular,
		// an ambiguous MCP name must keep its diagnostic and cannot suggest a bypass.
		if _, aliasErr := e.toolRegistry.ResolveWithAliases(name); aliasErr == nil {
			return nil, fmt.Errorf("%w; Claw alias %q requires a bundle declaring requires.iterion >= %s (or use %q)", err, name, bundle.ToolAliasesSince, toolcatalog.BuiltinAlias(name))
		}
	}
	return td, err
}

// mcpServerNamingTools are the three claw builtins that do not belong to one
// MCP server but take the server's NAME as an argument the model writes.
// Every other MCP tool is registered per server as `mcp.<server>.<tool>` and
// is therefore scoped by checkNodeToolAccess; these three are not, and they
// reach the launcher's MCP provider, which connects the named server — for a
// stdio server, starts its process.
// Read from the registrar that creates them, never hand-copied: a second
// copy is a list that will one day be shorter than the first, silently, for
// exactly one new tool — and the placement table (which has its own
// exhaustiveness guard) would happily accept that tool as launcher-placed
// while this scope check quietly did not apply to it.
var mcpServerNamingTools = func() map[string]bool {
	out := map[string]bool{}
	for _, name := range tool.MCPServerNamingTools() {
		out[name] = true
	}
	return out
}()

// withholdUnscopedMCPServerNamingTool reports whether one of the three is to
// be left out of an LLM node's tool set entirely, because that node's active
// MCP set is empty: scopeMCPServerNamingTool would then refuse every call the
// model could make.
//
// Advertising a tool whose every invocation is refused is worse than not
// advertising it: the model spends turns discovering that, and a model that
// keeps retrying reads the refusal as a transient error. The same reasoning
// is already why buildSubagentTools withholds these three from a child
// conversation that has no node to be scoped by.
func (e *ClawExecutor) withholdUnscopedMCPServerNamingTool(t delegate.ToolDef, node ir.Node) bool {
	if !mcpServerNamingTools[t.Name] || node == nil {
		return false
	}
	if _, isLLM := node.(ir.LLMNode); !isLLM {
		return false
	}
	if len(nodeActiveMCPServers(node)) > 0 {
		return false
	}
	if e.logger != nil {
		e.logger.Info("[%s] tool %q withheld: the node has no active MCP server for it to name",
			node.NodeID(), t.Name)
	}
	return true
}

// scopeMCPServerNamingTool restricts those three to the node's own active MCP
// servers.
//
// checkNodeToolAccess cannot serve here: it reads an EMPTY active set as
// "unrestricted" (correct for a tool node, which has no MCP scope at all),
// which for an LLM node that declares `inherit: false` says the opposite of
// what the author wrote.
//
// So the distinction is drawn on the NODE, not on the set: a non-LLM node is
// left alone, and an LLM node is held to its active set. The empty set still
// denies every server here, deliberately, even though
// withholdUnscopedMCPServerNamingTool means resolution does not normally
// deliver a tool in that state: the wrapper travels ON the ToolDef, so it is
// what guards the call if any other path ever hands one out.
func (e *ClawExecutor) scopeMCPServerNamingTool(t delegate.ToolDef, node ir.Node) delegate.ToolDef {
	if !mcpServerNamingTools[t.Name] || node == nil {
		return t
	}
	if _, isLLM := node.(ir.LLMNode); !isLLM {
		return t
	}
	allowed := nodeActiveMCPServers(node)
	original := t.Execute
	toolName := t.Name
	nodeID := node.NodeID()
	t.Execute = func(ctx context.Context, input json.RawMessage) (string, error) {
		// Decoded EXACTLY as the tool underneath decodes it: into a map, read
		// by the literal key, with the same default. A struct field here
		// instead read `{"server":"forbidden","Server":"allowed"}` as
		// "allowed" — encoding/json matches field names case-insensitively
		// and the last matching key wins — while the callee, reading
		// input["server"], saw "forbidden". A guard and the call it guards
		// must read the same bytes the same way, or the guard judges a value
		// nobody uses.
		var args map[string]any
		if len(input) > 0 && string(input) != "null" {
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("model: node %q: %s: decode input: %w", nodeID, toolName, err)
			}
		}
		// claw defaults a missing (or non-string) server to "default"; mirror
		// that too, so a body it would resolve to "default" is judged as
		// "default" rather than refused for a reason the model cannot act on.
		server := "default"
		if s, ok := args["server"].(string); ok && s != "" {
			server = s
		}
		for _, name := range allowed {
			if name == server {
				return original(ctx, input)
			}
		}
		return "", fmt.Errorf("model: node %q cannot reach MCP server %q through %s: the node's active MCP servers are %v",
			nodeID, server, toolName, allowed)
	}
	return t
}

// collectRefusedMCPServers ensures the node's active MCP servers for the
// given tool names and folds every launcher-start refusal into `refused`
// (server → reason). Any other failure is returned as before: a server this
// launcher may start and cannot boot is a broken dependency, and the node
// must fail on it.
func (e *ClawExecutor) collectRefusedMCPServers(ctx context.Context, node ir.Node, names []string, refused map[string]string) error {
	if e.mcpManager == nil || e.toolRegistry == nil {
		return nil
	}
	for _, server := range activeMCPServersForNames(node, names) {
		if _, already := refused[server]; already {
			continue
		}
		err := e.mcpManager.EnsureServers(ctx, e.toolRegistry, []string{server})
		if err == nil {
			continue
		}
		if mcp.ServerNotStartable(err) {
			refused[server] = err.Error()
			continue
		}
		return err
	}
	return nil
}

// refusedMCPServerFor reports whether a tool name belongs to a server the
// launcher refused to start, in either spelling the registry resolves.
// Non-MCP names never match.
//
// Every reading of an ambiguous FQN is checked, and a refusal on any of them
// withholds the tool: `mcp__a__b__c` names server `a` or `a__b` depending on
// where the tool name starts, and a guard that guesses one advertises a tool
// whose server this launcher never started.
func refusedMCPServerFor(name string, refused map[string]string) bool {
	if len(refused) == 0 {
		return false
	}
	for _, server := range tool.MCPServerCandidatesOf(name) {
		if _, refusedHere := refused[server]; refusedHere {
			return true
		}
	}
	return false
}

// mcpServerActiveForNode reports whether `server` is in the node's active MCP
// set. A node with no set (a tool node, or an LLM node inheriting everything)
// is not restricted here — checkNodeToolAccess owns that rule; this is only
// about not STARTING a server the node cannot reach anyway.
func mcpServerActiveForNode(node ir.Node, server string) bool {
	active := nodeActiveMCPServers(node)
	if len(active) == 0 {
		return true
	}
	for _, name := range active {
		if name == server {
			return true
		}
	}
	return false
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
		var candidates []string
		// Support wildcard patterns like "mcp.claude_code.*".
		if tool.IsMCPWildcard(name) {
			s, err := tool.ParseMCPWildcard(name)
			if err != nil {
				continue
			}
			candidates = []string{s}
		} else {
			// Both spellings the registry resolves: a node that names
			// `mcp__srv__tool` asks for the same server as one that names
			// `mcp.srv.tool`, and skipping it here left the server unensured
			// — and, once a refusal existed to carry, unrecorded, so the
			// node died at build with "unknown tool" instead of refusing at
			// execution where its fallbacks could serve it. An FQN whose
			// tool name itself contains "__" has several readings; the
			// node's own active set decides which one it meant.
			candidates = tool.MCPServerCandidatesOf(name)
		}
		for _, server := range candidates {
			if _, ok := active[server]; !ok {
				continue
			}
			if _, ok := seen[server]; ok {
				continue
			}
			seen[server] = struct{}{}
			servers = append(servers, server)
		}
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
func (e *ClawExecutor) guardTool(executionCtx context.Context, t delegate.ToolDef, node ir.Node, qualifiedName string) delegate.ToolDef {
	original := t.Execute
	name := t.Name
	policy := e.toolPolicy
	t.Execute = func(ctx context.Context, input json.RawMessage) (string, error) {
		if err := policy.CheckContext(e.toolPolicyContext(ctx, executionCtx, node, name, qualifiedName, input)); err != nil {
			return "", err
		}
		return original(ctx, input)
	}
	return t
}

// toolPolicyContext is the one PolicyContext a node's tool is checked under,
// whether the check runs at the call (guardTool) or ahead of it (a tool a
// sandboxed runner executes in-container). callCtx carries the call's
// cancellation; buildCtx resolves the policy's alias patterns.
func (e *ClawExecutor) toolPolicyContext(callCtx, buildCtx context.Context, node ir.Node, name, qualifiedName string, input json.RawMessage) tool.PolicyContext {
	return tool.PolicyContext{
		Ctx:               callCtx,
		NodeID:            node.NodeID(),
		NodeKind:          node.NodeKind().String(),
		ToolName:          name,
		QualifiedToolName: qualifiedName,
		Input:             input,
		Vars:              e.vars,
		ResolvePattern:    e.policyPatternResolver(buildCtx, node),
	}
}

func (e *ClawExecutor) policyPatternResolver(ctx context.Context, node ir.Node) func(string) (string, error) {
	if !tool.BuiltinAliasesEnabled(ctx) {
		return nil
	}
	return func(pattern string) (string, error) {
		if toolcatalog.BuiltinAlias(pattern) == "" {
			return pattern, nil
		}
		if e.toolRegistry == nil {
			return "", fmt.Errorf("no tool registry configured")
		}
		definition, err := e.resolveToolReference(ctx, pattern)
		if err != nil {
			return "", err
		}
		if err := e.checkNodeToolAccess(node, definition.QualifiedName); err != nil {
			return "", err
		}
		return definition.QualifiedName, nil
	}
}

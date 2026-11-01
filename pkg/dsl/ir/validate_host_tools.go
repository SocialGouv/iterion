package ir

import (
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/boardcaps"
	"github.com/SocialGouv/iterion/pkg/runops"
)

// validateHostToolCapabilities closes the learn-at-run-time gap of the
// capability gate (#1801): since the host-tool families (board, watch, runs)
// open ONLY through a `capabilities:` grant, a node naming one of their tools
// in `tools:` — or a tool node whose bare `command:` resolves to one —
// without the capability learns it at run time: the grant filters
// registration, so the tool is absent from the registry (an agent loses it to
// a process-log line, a tool node's dispatch fails on "unknown tool"), and a
// call that still reaches boardops is refused on
// boardops.ErrCapabilityDenied. The compiler can say it earlier, and say what
// to declare.
//
// Warnings, never refusals: a name is matched against the KNOWN host tools —
// an unresolvable entry is C135's to judge, not this check's. The sanitized
// single-underscore form (`mcp_iterion_board_create_issue`) is not a `tools:`
// spelling at all — it is the shape iterion advertises to the provider for
// the MODEL's calls (canonicalMCPToolName), and no resolver reads it back
// from a list — so it stays C135's here too rather than drawing a second
// warning whose remedy (declare the capability) would leave the entry dead.
//
// The effective grant of an agent/judge is its own `capabilities:` when the
// list is declared (a declared `capabilities: []` holds NOTHING — the trap
// the rest of #1801 closed), else the workflow's. A tool node — and its
// rung-4 recovery agent, whose synthetic node carries no list of its own —
// holds the workflow's, so the remedy names the workflow's list.
func (c *compiler) validateHostToolCapabilities(w *Workflow) {
	wfCaps := capSet(w.Capabilities)
	for _, n := range w.Nodes {
		switch node := n.(type) {
		case *AgentNode:
			c.validateLLMHostTools(w, n, node.ID, "agent", node.Tools, effectiveCaps(node.Capabilities, wfCaps))
		case *JudgeNode:
			c.validateLLMHostTools(w, n, node.ID, "judge", node.Tools, effectiveCaps(node.Capabilities, wfCaps))
		case *ToolNode:
			// A tool node's registry recipe names a registered tool exactly —
			// but a bare command still resolves by unique suffix over every
			// registered MCP tool, so workflow-level MCP wiring makes it as
			// ambiguous as an agent's bare entry.
			if ToolRecipeKindOf(node) == ToolRecipeRegistry && !mcpWiringDecidesBareNames(w, node) {
				c.warnHostToolCap(node.ID, "tool", strings.TrimSpace(node.Command), "", wfCaps, "the workflow's", nil)
			}
			// The rung-4 recovery agent's agent_tools: is the third list the
			// runtime gates: the synthetic node carries no capabilities of its
			// own and inherits the workflow's (executor_build_task), so a host
			// tool named there without the grant is dropped after the
			// deterministic rungs already failed. C135/C159 cover this list;
			// C328 does too.
			if node.Recovery != nil {
				seen := map[string]bool{}
				for _, entry := range node.Recovery.AgentTools {
					if mcpWiringDecidesBareNames(w, node) && entry == hostToolBareName(entry) && !strings.HasSuffix(strings.TrimSpace(entry), "*") {
						continue
					}
					for _, ref := range expandHostToolEntry(entry) {
						c.warnHostToolCap(node.ID, "tool", ref, ": recovery agent_tools:", wfCaps, "the workflow's", seen)
					}
				}
			}
		}
	}
}

// validateLLMHostTools checks each entry of an LLM node's tools: list against
// the node's effective grant. Bare names step aside where MCP wiring is
// visible; the qualified spellings stay judged either way.
func (c *compiler) validateLLMHostTools(w *Workflow, n Node, nodeID, kind string, tools []string, caps map[string]bool) {
	bareJudged := !mcpWiringDecidesBareNames(w, n)
	seen := map[string]bool{}
	for _, entry := range tools {
		if !bareJudged && entry == hostToolBareName(entry) && !strings.HasSuffix(strings.TrimSpace(entry), "*") {
			continue
		}
		for _, ref := range expandHostToolEntry(entry) {
			c.warnHostToolCap(nodeID, kind, ref, "", caps, "its", seen)
		}
	}
}

// mcpWiringDecidesBareNames reports whether compile-time-visible MCP wiring
// makes a bare `tools:` entry (or a tool node's bare `command:`) ambiguous
// with a third-party server's own tool: a bare name resolves by unique suffix
// over every registered MCP tool, and when such a server may be connected a
// bare `subscribe` may be THEIRS — only the qualified spellings stay judged.
//
// Stricter than mcpWiringVisible (C135) on one point: a top-level
// `mcp_server:` DECLARATION alone wires nothing — a server reaches a node's
// registry only when an `mcp:` block names it (servers:), inherits or
// autoloads the ambient set, or disables one of it (PrepareWorkflow folds
// only those into the node's active set). A declaration no block activates
// leaves the bare name iterion's own shorthand, and the check keeps its
// teeth. C135's severity softening can afford the looser reading; this
// check's silence cannot.
func mcpWiringDecidesBareNames(w *Workflow, n Node) bool {
	if w != nil && mcpBlockWires(w.MCP) {
		return true
	}
	switch nn := n.(type) {
	case *AgentNode:
		return mcpBlockWires(nn.MCP)
	case *JudgeNode:
		return mcpBlockWires(nn.MCP)
	}
	return false
}

// warnHostToolCap emits C328 when the named host tool's capability is not in
// caps. surface names the list the entry was read from when it is not the
// node's own tools: (": recovery agent_tools:"). owner words whose list the
// remedy edits ("its" for an agent/judge, "the workflow's" for a tool node).
// seen deduplicates on the resolved bare name: one warning per host tool per
// list, however many spellings name it (nil = no dedup — the single-name
// call sites).
func (c *compiler) warnHostToolCap(nodeID, kind, toolName, surface string, caps map[string]bool, owner string, seen map[string]bool) {
	cap, ok := HostToolCapability(toolName)
	if !ok || caps[cap] {
		return
	}
	if seen != nil {
		bare := hostToolBareName(toolName)
		if seen[bare] {
			return
		}
		seen[bare] = true
	}
	c.warnfAt(DiagHostToolWithoutCapability, nodeID, "",
		"%s %q%s names the host tool %q but holds no %q capability — at run time the tool is dropped or its call refused. Declare %q in %s capabilities: list",
		kind, nodeID, surface, toolName, cap, cap, owner)
}

// HostToolCapability maps a host tool reference — in any of the spellings the
// runtimes accept in a `tools:` list (bare shorthand,
// `mcp.iterion_<family>.<tool>`, `mcp__iterion_<family>__<tool>`) — to the
// capability that opens it. The bool is false for a name that is no host
// tool. Wildcards are not a name: expandHostToolEntry fans them out first.
func HostToolCapability(name string) (string, bool) {
	bare := hostToolBareName(name)
	if cap, ok := boardcaps.ToolCapability[bare]; ok {
		return cap, true
	}
	switch bare {
	case "subscribe":
		return CapWatchSubscribe, true
	case "unsubscribe":
		return CapWatchUnsubscribe, true
	case "run_events", "run_get", "runs_list":
		return runops.CapRunsRead, true
	}
	return "", false
}

// hostToolBareName strips the MCP server qualification from a tools: entry.
// Only iterion's own server names qualify — a dotted or FQN name over another
// server (mcp.github.*) is not a host tool whatever its leaf says.
func hostToolBareName(name string) string {
	name = strings.TrimSpace(name)
	for _, p := range []string{"mcp__iterion_", "mcp.iterion_"} {
		rest, found := strings.CutPrefix(name, p)
		if !found {
			continue
		}
		// <family>__<tool> or <family>.<tool>
		if i := strings.Index(rest, "__"); i > 0 {
			return rest[i+2:]
		}
		if i := strings.Index(rest, "."); i > 0 {
			return rest[i+1:]
		}
		return rest
	}
	return name
}

// expandHostToolEntry fans a tools: entry out to the host tool names it
// covers: a family wildcard (`mcp.iterion_board.*`, `mcp__iterion_board__*`)
// expands to the whole family, anything else is itself. The family name
// lists are the same literals HostToolCapability maps — the conformance test
// in this package (TestC328_HostToolNamesMatchTheShorthandCatalog) pins both to
// toolcatalog's shorthand set, itself pinned to the live registry.
func expandHostToolEntry(entry string) []string {
	entry = strings.TrimSpace(entry)
	if !strings.HasSuffix(entry, "*") {
		return []string{entry}
	}
	family := ""
	for _, p := range []string{"mcp__iterion_", "mcp.iterion_"} {
		if rest, found := strings.CutPrefix(entry, p); found {
			// `<family>.*` or `<family>__*`
			family = strings.TrimRight(strings.TrimSuffix(rest, "*"), "_.")
			break
		}
	}
	var out []string
	switch family {
	case "board":
		for name := range boardcaps.ToolCapability {
			out = append(out, name)
		}
	case "watch":
		out = []string{"subscribe", "unsubscribe"}
	case "runs":
		out = []string{"run_events", "run_get", "runs_list"}
	default:
		return []string{entry}
	}
	sort.Strings(out)
	return out
}

// capSet reads a declared list as a set. nil and empty differ upstream (a
// declared `capabilities: []` holds nothing); both read as "no grant" here,
// which is the reading this check needs.
func capSet(caps []string) map[string]bool {
	set := make(map[string]bool, len(caps))
	for _, c := range caps {
		set[strings.TrimSpace(c)] = true
	}
	return set
}

// effectiveCaps is the node's declared list when it has one (a declared empty
// list is a declaration of nothing), else the workflow's.
func effectiveCaps(nodeCaps []string, wfCaps map[string]bool) map[string]bool {
	if nodeCaps != nil {
		return capSet(nodeCaps)
	}
	return wfCaps
}

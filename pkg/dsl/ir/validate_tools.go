package ir

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// Tool-list diagnostics.
const (
	DiagUnknownTool DiagCode = "C135" // a `tools:` entry cannot resolve on a backend the list actually constrains (error)
)

// validateNodeTools checks that every declared tool name can actually resolve
// — but only where the list is a real constraint. Two lists carry such names:
// an agent/judge node's `tools:`, and a Verified Action's rung-4
// `recovery: agent_tools:` (see validateRecoveryAgentTools).
//
// The failure this closes is deterministic from the source and costs a model
// call to discover: `tools: [read_file, list_files]` on claw compiles, the run
// starts, the workspace is prepared, and the first LLM node dies on
// `unknown tool "list_files"`. Nothing about that is knowable only at run time.
//
// Two properties keep it from firing on workflows that work:
//
//   - it applies to claw alone (toolcatalog.ConstrainsTools). Every CLI
//     backend runs its own native toolset — claude_code ignores the lowercase
//     list entirely under bypassPermissions — so an unresolvable name there is
//     dead config, not a failure, and an error would be plain wrong;
//   - it decides only bare built-in names (toolcatalog.IsStaticBuiltinRef).
//     `mcp.<server>.<tool>` entries, the `mcp__server__tool` alias form,
//     wildcards and `${VAR}` refs are resolved when the server connects or the
//     env is read, so the compiler has no opinion on them.
//
// A node whose effective backend is unresolved (no `backend:`, no
// `default_backend:`) is left alone for the same reason validateAutoMemory
// leaves it alone: the resolver falls through to env and host credential
// detection, so the compiler genuinely cannot know which backend will serve —
// and on the likeliest one (claude_code) the list is inert.
func (c *compiler) validateNodeTools(w *Workflow) {
	report := c.toolDiagReporter(w)
	for _, n := range w.Nodes {
		if tn, ok := n.(*ToolNode); ok {
			c.validateRecoveryAgentTools(w, tn)
			continue
		}
		nn, ok := n.(LLMNode)
		if !ok {
			continue
		}
		unresolvable := unresolvableToolNames(nn.GetTools())
		if len(unresolvable) == 0 {
			continue
		}
		kind, id := nn.NodeKind().String(), nn.NodeID()

		// The node's own backend first: this is the route it takes on every
		// ordinary run.
		backend := effectiveNodeBackend(nn.GetLLMFields().Backend, w.DefaultBackend)
		if toolcatalog.ConstrainsTools(backend) {
			for _, name := range unresolvable {
				report(DiagUnknownTool, id, "",
					"%s %q: tools: names %q, which backend %q cannot resolve — the node fails the moment it dispatches%s",
					kind, id, name, backend, toolHint(name))
			}
			continue
		}

		// The node does not run on claw, but a `fallbacks:` route might. The
		// list is the SAME list there, and a route exists precisely to serve
		// when the primary is already failing — so an unresolvable name turns
		// the safety net into a second failure. Reported once, against the
		// first claw route: every route shares this list, so repeating the
		// message per route would say nothing new.
		for _, fb := range nn.GetFallbacks() {
			if !toolcatalog.ConstrainsTools(fb.Backend) {
				continue
			}
			for _, name := range unresolvable {
				report(DiagUnknownTool, id, "",
					"%s %q: tools: names %q, which fallback %s cannot resolve on backend %q — the route would fail at the moment the run is already falling back%s",
					kind, id, name, fallbackLabel(fb), fb.Backend, toolHint(name))
			}
			break
		}
	}
}

// validateRecoveryAgentTools covers the other place a declared tool list
// reaches a backend: a Verified Action's rung-4 recovery agent (ADR-044).
// executor_verified_action hand-builds an ir.AgentNode carrying `agent_tools:`
// verbatim, so the same names go through the same registry resolution — and
// the same `unknown tool` failure, at the worst possible moment, since rung 4
// runs only after the deterministic rungs have already failed.
//
// The synthetic node declares no backend of its own, so only the workflow
// default can make it claw; anything else stays unresolved and silent, exactly
// as for a node's own `tools:`.
func (c *compiler) validateRecoveryAgentTools(w *Workflow, tn *ToolNode) {
	if tn == nil || tn.Recovery == nil {
		return
	}
	backend := effectiveNodeBackend("", w.DefaultBackend)
	if !toolcatalog.ConstrainsTools(backend) {
		return
	}
	report := c.toolDiagReporter(w)
	for _, name := range unresolvableToolNames(tn.Recovery.AgentTools) {
		report(DiagUnknownTool, tn.ID, "",
			"tool %q: recovery agent_tools: names %q, which backend %q cannot resolve — the rung-4 recovery agent fails after the deterministic rungs already have%s",
			tn.ID, name, backend, toolHint(name))
	}
}

// toolDiagReporter picks C135's severity for this workflow.
//
// The finding is an ERROR by default: the name cannot resolve, so the node
// cannot run. It degrades to a WARNING when the workflow wires MCP servers of
// its own, because Registry.Resolve matches a dot-free name as a unique suffix
// over the connected servers — and those servers' tool lists exist only once
// they connect. Blocking there would reject a workflow that runs, which is the
// expensive direction: the author keeps a real signal, and the qualified
// `mcp.<server>.<tool>` form settles it either way.
//
// iterion's own board/watch tools do NOT go through this softening: their
// names are fixed and known, so unresolvableToolNames accepts them outright
// and the check keeps its teeth on every other name.
func (c *compiler) toolDiagReporter(w *Workflow) func(DiagCode, string, string, string, ...any) {
	if w != nil && len(w.MCPServers) > 0 {
		return c.warnfAt
	}
	return c.errorfAt
}

// unresolvableToolNames returns the entries of a `tools:` list that name a
// built-in the registry does not have, in declaration order and deduplicated.
func unresolvableToolNames(tools []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range tools {
		name := strings.TrimSpace(t)
		if !toolcatalog.IsStaticBuiltinRef(name) || toolcatalog.IsBuiltin(name) || seen[name] {
			continue
		}
		// iterion's own board / watch tools are registered under the MCP
		// namespace but reachable by their bare name through Registry.Resolve's
		// shorthand path, and the runtime registers them for every run.
		if toolcatalog.ResolvesViaShorthand(name) {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// toolHint renders the trailing advice for one unresolvable name: the nearest
// built-in when there is a plausible one, and otherwise a pointer to the list.
// Returned with its own leading punctuation so the caller's format string
// reads as one sentence either way.
func toolHint(name string) string {
	if toolcatalog.IsUnexpandedRef(name) {
		return ". Tool names are the one field iterion does not expand — unlike model:, backend: and command:, a `${VAR}` or `{{ref}}` entry reaches the registry verbatim; name the tool literally"
	}
	if suggestion := toolcatalog.Suggest(name); suggestion != "" {
		return fmt.Sprintf(". Did you mean %q?", suggestion)
	}
	return ". claw resolves bare names against its own built-ins (read_file, write_file, file_edit, glob, grep, bash, web_fetch, …); an MCP tool is named `mcp.<server>.<tool>`"
}

// unresolvableToolsReason returns why a route may not serve this node's tools
// list, or "" when it may. Shared with ApplyRunFallback so an operator cannot
// reach through `--fallback` the route the compiler refuses in the .bot — the
// same pairing as ungatedCrossingReason / toolsInversionReason.
func unresolvableToolsReason(routeBackend string, tools []string) string {
	if !toolcatalog.ConstrainsTools(routeBackend) {
		return ""
	}
	unresolvable := unresolvableToolNames(tools)
	if len(unresolvable) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"runs on backend %q, which cannot resolve this node's declared tool(s) %s — the route would fail at the moment the run is already falling back",
		routeBackend, quoteJoin(unresolvable))
}

// quoteJoin renders names as `"a", "b"` for a diagnostic.
func quoteJoin(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted, ", ")
}

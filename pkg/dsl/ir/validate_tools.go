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
				d := c.toolDiagReporter(w, n, name)
				d.report(DiagUnknownTool, id, "",
					"%s %q: tools: names %q, which backend %q %s%s",
					kind, id, name, backend, d.consequence(), d.hint(name))
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
				d := c.toolDiagReporter(w, n, name)
				d.report(DiagUnknownTool, id, "",
					"%s %q: tools: names %q, which fallback %s %s on backend %q — the route would fail at the moment the run is already falling back%s",
					kind, id, name, fallbackLabel(fb), d.routeConsequence(), fb.Backend, d.hint(name))
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
	for _, name := range unresolvableToolNames(tn.Recovery.AgentTools) {
		d := c.toolDiagReporter(w, tn, name)
		d.report(DiagUnknownTool, tn.ID, "",
			"tool %q: recovery agent_tools: names %q, which backend %q %s on the rung-4 recovery agent — it would fail after the deterministic rungs already have%s",
			tn.ID, name, backend, d.routeConsequence(), d.hint(name))
	}
}

// toolDiagReporter picks C135's severity for ONE unresolvable name.
//
// It BLOCKS only what the compiler can positively identify as wrong
// (toolcatalog.IsIdentifiableMistake: a legacy phantom name, a near-miss typo,
// an unexpandable `${VAR}`) — and only where no MCP wiring is in sight.
// Everything else warns.
//
// The reason no bare name can be blocked on principle is that Registry.Resolve
// matches a dot-free name as a unique suffix over every connected MCP server,
// and half that catalog is invisible here: project `.mcp.json` entries and
// enabled plugins' `mcp_servers` are merged by pkg/backend/mcp.PrepareWorkflow,
// which runs AFTER ir.Compile. A claw node also gets those servers spliced in
// as `mcp.<srv>.*` (executor_build_task.go), so `tools: [firecrawl_search]`
// resolves and runs on a host with that plugin enabled. Refusing it would be
// the expensive direction of drift — a run-blocking error on a workflow that
// works — to guard a guess.
//
// Visible MCP wiring (an `mcp_server:` declaration, or an `mcp:` activation
// block on the workflow or the node) softens even the identifiable names: a
// server the author is deliberately wiring can carry any name at all,
// `list_files` included.
//
// iterion's own board/watch tools never reach this function: their names are
// fixed and known, so unresolvableToolNames accepts them outright and the
// check keeps its teeth on every other name.
func (c *compiler) toolDiagReporter(w *Workflow, n Node, name string) toolDiag {
	if toolcatalog.IsIdentifiableMistake(name) && !mcpWiringVisible(w, n) {
		return toolDiag{report: c.errorfAt, blocking: true}
	}
	return toolDiag{report: c.warnfAt}
}

// toolDiag carries C135's severity for one name together with the wording it
// licenses. A warning that reads "the node fails the moment it dispatches"
// asserts the certainty the severity just declined — so the consequence clause
// travels with the reporter rather than being written once and hedged later.
type toolDiag struct {
	report   func(DiagCode, string, string, string, ...any)
	blocking bool
}

// consequence renders what happens on the node's own backend.
func (d toolDiag) consequence() string {
	if d.blocking {
		return "cannot resolve — the node fails the moment it dispatches"
	}
	return "does not have — unless a connected MCP server supplies it, the node fails the moment it dispatches"
}

// routeConsequence is the same for a `fallbacks:` route, where the failure
// lands at the worst possible moment.
func (d toolDiag) routeConsequence() string {
	if d.blocking {
		return "cannot resolve"
	}
	return "may not resolve"
}

// hint appends the trailing advice: the way out on the fatal branch, and on
// the other one why the finding stopped short of blocking.
//
// The blocking branch needs the escape hatch MOST, not least. Its predicate
// includes "within an edit or two of a real built-in", which is overwhelmingly
// a typo but can also be an ambient MCP tool that happens to sit that close to
// one (a server exposing `grepp`, `task_lists`); there the diagnostic is fatal
// AND wrong, and an author told only `Did you mean "grep"?` has no visible
// remedy for a run the compiler just refused.
func (d toolDiag) hint(name string) string {
	h := toolHint(name)
	if d.blocking {
		return h + " If the name really is an MCP server's tool, spell it `mcp.<server>.<tool>` — or declare the server (a top-level `mcp_server:`, or an `mcp:` block on the workflow or the node), which softens this to a warning"
	}
	return h + " Reported as a warning, not an error: a bare name also resolves onto an MCP tool when it is unique across the connected servers, and the ambient catalog (a project .mcp.json, an enabled plugin) is merged after compilation — name it `mcp.<server>.<tool>` to be explicit"
}

// mcpWiringVisible reports whether the source shows the author wiring MCP at
// all — the signal that a bare name may legitimately be a server's tool.
//
// It reads the two DSL surfaces the compiler has: top-level `mcp_server:`
// declarations (w.MCPServers) and the `mcp:` activation blocks on the workflow
// and the node (w.MCP / AgentNode.MCP / JudgeNode.MCP). It cannot see the
// ambient catalog — that is why an unidentifiable name warns even here.
func mcpWiringVisible(w *Workflow, n Node) bool {
	if w != nil && (len(w.MCPServers) > 0 || w.MCP != nil) {
		return true
	}
	switch nn := n.(type) {
	case *AgentNode:
		return nn.MCP != nil
	case *JudgeNode:
		return nn.MCP != nil
	}
	return false
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
		return ". Tool names are the one field iterion does not expand — unlike model:, backend: and command:, a `${VAR}` or `{{ref}}` entry reaches the registry verbatim; name the tool literally."
	}
	if suggestion := toolcatalog.Suggest(name); suggestion != "" {
		return fmt.Sprintf(". Did you mean %q?", suggestion)
	}
	return ". claw resolves bare names against its own built-ins (read_file, write_file, file_edit, glob, grep, bash, web_fetch, …); an MCP tool is named `mcp.<server>.<tool>`."
}

// unresolvableToolsReason returns why a route may not serve this node's tools
// list, or "" when it may. Shared with ApplyRunFallback so an operator cannot
// reach through `--fallback` the route the compiler refuses in the .bot — the
// same pairing as UngatedCrossingReason / toolsInversionReason.
//
// mcpVisible carries the caller's view of the node's MCP wiring; together with
// IsIdentifiableMistake it reproduces exactly what C135 BLOCKS, so the flag
// and the .bot are refused on the same terms and never on a guess.
func unresolvableToolsReason(routeBackend string, tools []string, mcpVisible bool) string {
	if !toolcatalog.ConstrainsTools(routeBackend) || mcpVisible {
		return ""
	}
	var unresolvable []string
	for _, name := range unresolvableToolNames(tools) {
		if toolcatalog.IsIdentifiableMistake(name) {
			unresolvable = append(unresolvable, name)
		}
	}
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

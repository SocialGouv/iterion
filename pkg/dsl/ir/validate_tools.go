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

// validateNodeTools checks that every name in an agent/judge node's `tools:`
// list can actually resolve — but only where the list is a real constraint.
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
				c.errorfAt(DiagUnknownTool, id, "",
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
				c.errorfAt(DiagUnknownTool, id, "",
					"%s %q: tools: names %q, which fallback %s cannot resolve on backend %q — the route would fail at the moment the run is already falling back%s",
					kind, id, name, fallbackLabel(fb), fb.Backend, toolHint(name))
			}
			break
		}
	}
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

package toolcatalog

import "strings"

// ToolsDeclared reports whether the author declared a tool surface on the
// node — the ONE definition of that fact, so no reader re-derives it.
//
// `tools: []` is a declared value: the author wrote "this node has no tools".
// A node with no `tools:` line at all leaves the surface UNDECLARED, which
// every CLI backend reads as "no restriction". The two are carried apart by
// nilness from the parser down, which is why the parse arm for `tools:`
// returns a non-nil empty slice for `[]` while every other bracket list
// returns nil.
//
// The distinction stops at this predicate: `delegate.Task` carries the answer
// as a bool, so no backend re-derives nilness from a slice that crossed a
// JSON seam.
func ToolsDeclared(tools []string) bool { return tools != nil }

// ReceivesToolList reports whether a node's `tools:` list reaches a backend
// at all — the precondition for the list to bound anything there.
//
//   - claw resolves its ToolDefs from the list (but re-populates an empty
//     one with `ask_user` when `interaction:` is set, and the appends below
//     it follow);
//   - claude_code turns it into --disallowedTools over `claudeNativeTools`;
//   - codex maps it onto a sandbox mode.
//
// pi, kimi, grok and opencode are driven through the CLI-agent seam, which
// never passes the list to the agent: there a declared bound is silently
// dropped, which is what C270 warns about for a declared-empty list (a
// non-empty one is dropped just as silently, with no diagnostic at all).
//
// The default answer for an unknown backend is therefore the SAFE one — a
// backend is assumed not to receive the list until someone proves it does and
// names it here. `opencode` (#1640) is the worked example: it drives
// `opencode --format json run` with the prompt on stdin and reads no tool
// list at all, so `tools: [Bash, Read]` on such a node used to validate with
// zero diagnostics.
//
// It says the list ARRIVES, never that the node ends up holding nothing —
// and it is deliberately NOT a workspace-safety predicate. `claudeNativeTools`
// is a hardcoded enumeration of a roster iterion does not own: the same
// package's `orchestrationTools` (Agent, TaskOutput, Monitor) and
// `workflowOrchestrationTools` name tools outside it — `Agent`, `TaskOutput`
// and `Monitor` therefore survive a `tools: []`, and so does every MCP tool,
// which is not on the roster either. (`Workflow` is withheld separately, from
// every NON-ultracode node whatever its list; an ultracode node with
// `tools: []` keeps it.) On claw the runtime's
// own interaction and auto-memory appends can put `write_file` back. Anything that must know what
// a node can DO reads the effective surface, not this.
//
// It is also NOT ConstrainsTools, which answers a third question (can claw's
// registry resolve these bare names) and stays claw-only: widening that one
// turns advisory CLI-backend names into blocking C135 errors on shipped bots.
func ReceivesToolList(backend string) bool {
	switch strings.TrimSpace(backend) {
	case ClawBackend, "claude_code", "codex":
		return true
	default:
		return false
	}
}

package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestACodexRouteBoundsOnlyTheNamesItsSandboxReads walks the engine's
// read-only vocabulary name by name on codex, the one route that receives a
// `tools:` list and bounds the node by what the NAMES buy: the list picks a
// sandbox mode and narrows nothing else, codex's built-in shell running
// outside the SDK's per-name callbacks.
//
// The expectation comes from the REAL producer — delegate.CodexDeclaredListIsReadOnly,
// a wrapper over the classifier codex.go runs at its WithSandbox call — and
// never from a table of this test's own. A reading kinder than the delegate is
// what left a node declaring `git_diff` out of the prompt-injection class and
// admitted it to a shared worktree while the run handed it workspace-write
// (#1727); its own names are pinned delegate-side, in
// TestCodexDeclaredListIsReadOnlyAnswersFromTheNamesCodexReads.
//
// Both questions are asserted on every row: the boundary class is wider than
// shared-worktree admission on an UNNAMED route only (see unnamedRouteReading),
// and codex is a named one, so a cell where they differ here is a drift.
func TestACodexRouteBoundsOnlyTheNamesItsSandboxReads(t *testing.T) {
	const noBound = "codex maps the `tools:` list onto a sandbox mode, and what this node declares buys the workspace-write one, where codex's built-in shell acts on the worktree (#1727)"
	for _, name := range []string{
		"read_file", "glob", "grep", "web_fetch",
		"git_diff", "git_status", "list_files", "search_codebase", "tree",
	} {
		t.Run(name, func(t *testing.T) {
			if !IsReadOnlyTool(name) {
				t.Fatalf("precondition: %q is not in the engine's read-only vocabulary, so this row is about nothing", name)
			}
			tools := []string{name}
			sandboxed := delegate.CodexDeclaredListIsReadOnly(tools, true)
			node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "codex"}, Tools: tools}
			reason := ToolSurfaceWriteReason(node, "", authoredDefaults)
			if sandboxed != (reason == "") {
				t.Fatalf("tools: [%s] on codex: the delegate's sandbox is read-only=%v while the boundary question answers %q — one list, two readings, and the engine's is the one the catalogue guard trusts", name, sandboxed, reason)
			}
			if mutating := isMutatingNodeWithBackend(node, "", nil, nil); mutating == sandboxed {
				t.Errorf("tools: [%s] on codex: shared-worktree admission says mutating=%v for a node codex sandboxes read-only=%v — a node N branches share a worktree with must be read on the sandbox it gets", name, mutating, sandboxed)
			}
			if !sandboxed && reason != noBound {
				t.Errorf("tools: [%s] on codex: reason = %q, want %q — the message must name the arm that decided, so a reader can check it", name, reason, noBound)
			}
		})
	}
	// A route onto codex carries the node's list with it, and reads it the
	// same way: the primary that bounds those names does not make the route
	// bound them.
	withRoute := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "a"},
		LLMFields: ir.LLMFields{Backend: "codex"},
		Tools:     []string{"read_file"},
		Fallbacks: []ir.Fallback{{Name: "rescue", Backend: "codex"}},
	}
	if got := ToolSurfaceWriteReason(withRoute, "", authoredDefaults); got != "" {
		t.Errorf("a codex node declaring a name codex reads, with a codex route, holds its list on both: got %q", got)
	}
	withRoute.Tools = []string{"tree"}
	if got := ToolSurfaceWriteReason(withRoute, "", authoredDefaults); got != noBound {
		t.Errorf("the same two routes with a name codex does not read: got %q, want %q", got, noBound)
	}
	// A list declared EMPTY keeps the verdict an undeclared one reaches — the
	// pair TestADeclaredEmptyToolListReachesTheSameVerdictAsAnUndeclaredOne
	// walks — even though codex sandboxes it read-only: the runtime's own
	// appends put tools back on a list it cannot see here.
	declaredEmpty := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "codex"}, Tools: []string{}}
	if !delegate.CodexDeclaredListIsReadOnly([]string{}, true) {
		t.Fatal("precondition: codex sandboxes a declared-empty list read-only")
	}
	if got := ToolSurfaceWriteReason(declaredEmpty, "", authoredDefaults); got == "" {
		t.Error("`tools: []` on codex must keep the verdict an undeclared list reaches: the sandbox is read-only, and the appends it cannot see are why that is no proof")
	}
}

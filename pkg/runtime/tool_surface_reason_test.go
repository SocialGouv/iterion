package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestToolSurfaceWriteReasonNamesTheArmThatDecided pins the MESSAGE to the arm
// that produced the verdict. The reason is what a guard prints at the author,
// and a reason that names nothing — the dangling `tools: ` an all-read-only
// declared list used to produce — sends them looking for a tool they did not
// write.
func TestToolSurfaceWriteReasonNamesTheArmThatDecided(t *testing.T) {
	agent := func(backend string, tools []string, mutate func(*ir.AgentNode)) *ir.AgentNode {
		n := &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "a"},
			LLMFields: ir.LLMFields{Backend: backend},
			Tools:     tools,
		}
		if mutate != nil {
			mutate(n)
		}
		return n
	}
	readOnly := []string{"read_file", "glob"}
	for _, tc := range []struct {
		name string
		node ir.Node
		dflt string
		want string
	}{
		{
			name: "full_access answers before any list",
			node: agent("claw", readOnly, func(n *ir.AgentNode) { n.FullAccess = true }),
			want: "full_access",
		},
		{
			name: "a declared writer is named by the author's own line",
			node: agent("claude_code", []string{"read_file", "bash", "write_file"}, nil),
			want: "tools: bash, write_file",
		},
		{
			name: "a read-only list on claude_code",
			node: agent("claude_code", readOnly, nil),
			want: "claude_code takes the `tools:` list as --disallowedTools over a closed native roster iterion does not own, so a declared list bounds nothing (#1652)",
		},
		{
			name: "a read-only list on a backend the list never reaches",
			node: agent("kimi", readOnly, nil),
			want: "kimi never receives the `tools:` list, so a declared bound is dropped (#1652)",
		},
		{
			name: "a read-only list on a route no name resolves",
			node: agent("auto", readOnly, nil),
			want: `backend "auto" resolves at dispatch, where the compiled-in detection order's first choice is claude_code, which does not bound what the node holds (#1652)`,
		},
		{
			name: "no list at all on a route no name resolves",
			node: agent("", nil, nil),
			want: `backend "" resolves at dispatch, where the compiled-in detection order's first choice is claude_code, which does not bound what the node holds (#1652)`,
		},
		{
			// The verdict is the unnamed-route one; the MESSAGE names the
			// spelling the author wrote, not the nothing it expanded to.
			name: "a route spelled as a reference with no authored default",
			node: agent("${ITERION_TEST_REASON_BACKEND}", readOnly, nil),
			want: "backend ${ITERION_TEST_REASON_BACKEND} is read at its authored default, which names no route, so nothing here bounds what the node holds",
		},
		{
			name: "the same reference with no list at all",
			node: agent("${ITERION_TEST_REASON_BACKEND}", nil, nil),
			want: "backend ${ITERION_TEST_REASON_BACKEND} is read at its authored default, which names no route, so nothing here bounds what the node holds",
		},
		{
			name: "a route still spelled as a template",
			node: agent("{{vars.llm_backend}}", readOnly, nil),
			want: "backend {{vars.llm_backend}} is unresolved before the run, so no route it may take bounds what the node holds",
		},
		{
			// A node that declares NO list must not be told a declared one
			// bounds nothing: it goes looking for a `tools:` field it never
			// wrote. Live on docs-refresh/campaign.
			name: "a template route on a node that declares no list",
			node: agent("{{vars.llm_backend}}", nil, nil),
			want: "backend {{vars.llm_backend}} is unresolved before the run, so no route it may take bounds what the node holds",
		},
		{
			name: "a fallbacks: route that drops the list, under a primary that enforces it",
			node: agent("codex", readOnly, func(n *ir.AgentNode) {
				n.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: "grok"}}
			}),
			want: "a `fallbacks:` route onto grok never receives the `tools:` list, so a declared bound is dropped (#1652)",
		},
		{
			name: "a fallbacks: entry that names no backend inherits the node's, which bounds the list",
			node: agent("codex", readOnly, func(n *ir.AgentNode) {
				n.Fallbacks = []ir.Fallback{{Name: "skipped", Backend: ""}}
			}),
			want: "",
		},
		{
			// Nothing resolves a template here either, and the reason may not
			// name it as a backend and credit it with what it does to the list.
			name: "a fallbacks: route still spelled as a template",
			node: agent("codex", readOnly, func(n *ir.AgentNode) {
				n.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: "{{vars.fb}}"}}
			}),
			want: "a `fallbacks:` route still spelled as {{vars.fb}} is unresolved before the run, so no backend it may take bounds what the node holds",
		},
		{
			// The route the walk cannot read is the one it would otherwise
			// trust: a `${VAR}` with no authored default is the host's to
			// decide, so it is the unnamed-route cell, not "inherits".
			name: "a fallbacks: route spelled as a reference with no authored default",
			node: agent("codex", readOnly, func(n *ir.AgentNode) {
				n.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: "${ITERION_TEST_REASON_FALLBACK}"}}
			}),
			want: "a `fallbacks:` route onto ${ITERION_TEST_REASON_FALLBACK} is read at its authored default, which names no route, so nothing here bounds what the node holds",
		},
		{
			// An explicit `auto` on a ROUTE is a name the registry does not
			// have, so the route dies when the chain is needed and widens
			// nothing (ir.backendReader.routeName).
			name: "a fallbacks: route explicitly spelled auto",
			node: agent("codex", readOnly, func(n *ir.AgentNode) {
				n.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: "auto"}}
			}),
			want: "",
		},
		{
			// The authored default is read, so this route IS pi and the
			// message names pi rather than the spelling.
			name: "a fallbacks: route whose authored default names a backend the list never reaches",
			node: agent("codex", readOnly, func(n *ir.AgentNode) {
				n.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: "${ITERION_TEST_REASON_FALLBACK:-pi}"}}
			}),
			want: "a `fallbacks:` route onto pi never receives the `tools:` list, so a declared bound is dropped (#1652)",
		},
		{
			// The route is NAMED, so the message names it: a reader who has to
			// go find which of the node's routes was meant cannot check the
			// verdict.
			name: "claw with a CLI route runs the full native toolset whatever it declared",
			node: agent("claw", readOnly, func(n *ir.AgentNode) {
				n.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: "claude_code"}}
			}),
			want: "a `fallbacks:` route onto claude_code runs the full native toolset, which nothing the node declares bounds",
		},
		{
			// codex is the one readable route that does not run the full
			// native toolset on a declared list: it maps those names onto a
			// sandbox mode. The verdict is the claw→CLI one — a claw list is
			// the node's bound on claw alone — and the message says that,
			// rather than asserting a toolset the route will not run.
			name: "claw with a codex route whose sandbox the declared names buy",
			node: agent("claw", readOnly, func(n *ir.AgentNode) {
				n.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: "codex"}}
			}),
			want: "a `fallbacks:` route onto codex leaves claw, where the `tools:` list is the node's bound, and a pre-run reading does not credit the route's own sandbox mode as one",
		},
		{
			// The same route with a name codex does not read: there the full
			// native toolset IS what it runs, so the wording is the CLI one.
			name: "claw with a codex route the declared names leave workspace-write",
			node: agent("claw", []string{"git_diff"}, func(n *ir.AgentNode) {
				n.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: "codex"}}
			}),
			want: "a `fallbacks:` route onto codex runs the full native toolset, which nothing the node declares bounds",
		},
		{
			// A claw primary does not get its own wording for a route nothing
			// resolved: naming it "a CLI backend" and crediting it with the
			// full native toolset states as fact a route no executor has read.
			name: "claw with a route still spelled as a template",
			node: agent("claw", readOnly, func(n *ir.AgentNode) {
				n.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: "{{vars.fb}}"}}
			}),
			want: "a `fallbacks:` route still spelled as {{vars.fb}} is unresolved before the run, so no backend it may take bounds what the node holds",
		},
		{
			// The same claw primary and the same route, with NO list: the arm
			// that answers a declared list answers here too, or the absence of
			// a declaration comes out looser than a declaration.
			name: "claw with a route read at an authored default that does not exist, declaring no list",
			node: agent("claw", nil, func(n *ir.AgentNode) {
				n.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: "${ITERION_TEST_REASON_FALLBACK}"}}
			}),
			want: "a `fallbacks:` route onto ${ITERION_TEST_REASON_FALLBACK} is read at its authored default, which names no route, so nothing here bounds what the node holds",
		},
		{
			// `default_backend:` answers for such a reference only on a host
			// that names nothing, and any name the host does give wins, so
			// this reading credits the default's bound to no node: the message
			// names the node's own spelling, which is the field the author
			// must fix.
			name: "a reference with no authored default under a default_backend that names a route",
			node: agent("${ITERION_TEST_REASON_BACKEND}", readOnly, nil),
			dflt: "claw",
			want: "backend ${ITERION_TEST_REASON_BACKEND} is read at its authored default, which names no route, so nothing here bounds what the node holds",
		},
		{
			name: "no list on a CLI delegate",
			node: agent("codex", nil, nil),
			want: "no `tools:` list declared on codex, the full native toolset",
		},
		{
			// The verdict is the undeclared one, deliberately; denying the
			// author's own `tools: []` line in the message is not — and
			// neither is crediting codex with the native toolset, which the
			// delegate's own classifier does NOT hand a declared-empty list
			// (it sandboxes it read-only). The message names the engine rule
			// it applied, never a reading of the route the seam contradicts.
			name: "a declared-empty list on codex, where the delegate reads it as read-only",
			node: agent("codex", []string{}, nil),
			want: "`tools: []` on codex names no tool, which is no proof the node holds none: it reaches the verdict an undeclared list reaches, deliberately",
		},
		{
			name: "a read-only list on claw",
			node: agent("claw", readOnly, nil),
			want: "",
		},
		{
			name: "no list on claw",
			node: agent("claw", nil, nil),
			want: "",
		},
		{
			name: "a node kind with no prompt to bound",
			node: &ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}},
			dflt: "claude_code",
			want: "",
		},
	} {
		got := ToolSurfaceWriteReason(tc.node, tc.dflt, authoredDefaults)
		if got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
		if canWrite := ToolSurfaceCanWrite(tc.node, tc.dflt, authoredDefaults); canWrite != (tc.want != "") {
			t.Errorf("%s: ToolSurfaceCanWrite = %v but the reason is %q — the verdict and its cause come out of one walk", tc.name, canWrite, got)
		}
	}
}

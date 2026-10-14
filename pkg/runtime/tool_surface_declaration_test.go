package runtime

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestADeclaredListIsNoBoundWhereTheRouteDropsIt walks the two questions this
// file answers about ONE node — "what can it do" (the catalog's
// prompt-injection contract, ToolSurfaceCanWrite) and "may two branches share a
// worktree" (isMutatingNode) — over a node whose declared list is read-only.
//
// They read the same fact: on claude_code the list becomes `--disallowedTools`
// over a closed native roster iterion does not own, and the CLI-agent seam
// backends never receive it at all, so neither route is bounded BY the
// declaration. The two differ on ONE cell, and the table says which: a route no
// name resolves (`backend: ""`, `auto`) is read as unbounded for the wider
// question, where a wrong guess costs one paragraph in a system prompt, and
// left unread for shared-worktree admission, where it would refuse a fan-out
// that is safe.
func TestADeclaredListIsNoBoundWhereTheRouteDropsIt(t *testing.T) {
	// The reference rows below read the authored default on both questions:
	// admission reads the process environment, so the name must be empty
	// there too for the table to mean the same thing on every machine.
	t.Setenv("ITERION_TEST_DECL_BACKEND", "")
	readOnly := []string{"read_file", "glob", "grep"}
	for _, tc := range []struct {
		backend, dflt      string
		canWrite, mutating bool
		why                string
	}{
		{backend: "claude_code", canWrite: true, mutating: true, why: "claude_code takes the list as --disallowedTools over a roster iterion does not own"},
		{backend: "pi", canWrite: true, mutating: true, why: "the CLI-agent seam never passes the list to the agent"},
		{backend: "kimi", canWrite: true, mutating: true, why: "the CLI-agent seam never passes the list to the agent"},
		{backend: "grok", canWrite: true, mutating: true, why: "the CLI-agent seam never passes the list to the agent"},
		{backend: "opencode", canWrite: true, mutating: true, why: "the CLI-agent seam never passes the list to the agent"},
		{backend: "a_backend_nobody_wired_yet", canWrite: true, mutating: true, why: "an unknown backend is assumed not to receive the list until someone proves it does"},
		{backend: "claw", canWrite: false, mutating: false, why: "claw resolves its ToolDefs from the list"},
		// Name by name, against the delegate's own classifier:
		// TestACodexRouteBoundsOnlyTheNamesItsSandboxReads. These three are
		// read-only there; `git_diff` and the rest of the advisory vocabulary
		// buy the workspace-write sandbox and join the class.
		{backend: "codex", canWrite: false, mutating: false, why: "codex maps the list onto a sandbox mode it enforces, and these names buy the read-only one"},
		{backend: "", canWrite: true, mutating: false, why: "the one divergence: an unnamed route resolves at dispatch"},
		{backend: "auto", canWrite: true, mutating: false, why: "the one divergence: an unnamed route resolves at dispatch"},
		{dflt: "claude_code", canWrite: true, mutating: true, why: "the workflow's default fills in for a node that names no backend"},
		{dflt: "claw", canWrite: false, mutating: false, why: "the workflow's default fills in for a node that names no backend"},
		// A reference read at an authored default that does not exist names no
		// route here, and the workflow default is not the node's bound: at
		// dispatch the node's own spelling is read against the host FIRST and
		// any name it answers wins, `default_backend:` answering only on a host
		// that names none (model.ClawExecutor.resolveBackendName, steps 1-2).
		// Reading it as the default gave ONE authored spelling two verdicts,
		// decided by a field the run may never consult for it.
		{backend: "${ITERION_TEST_DECL_BACKEND}", dflt: "claw", canWrite: true, mutating: false, why: "the host may name any backend in a reference, and that name wins over default_backend:"},
		{backend: "${ITERION_TEST_DECL_BACKEND}", dflt: "codex", canWrite: true, mutating: false, why: "the host may name any backend in a reference, and that name wins over default_backend:"},
		{backend: "$ITERION_TEST_DECL_BACKEND", dflt: "claw", canWrite: true, mutating: false, why: "the unbraced spelling expands the same way, so it reaches the same cell"},
		// The control on that widening: an authored default that DOES name a
		// route is read, and the verdict is that route's.
		{backend: "${ITERION_TEST_DECL_BACKEND:-claw}", dflt: "claude_code", canWrite: false, mutating: false, why: "the authored default names claw, which bounds the list — a reference is not read pessimistically when it names a route"},
	} {
		node := &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "a"},
			LLMFields: ir.LLMFields{Backend: tc.backend},
			Tools:     readOnly,
		}
		if got := ToolSurfaceCanWrite(node, tc.dflt, authoredDefaults); got != tc.canWrite {
			t.Errorf("backend=%q default=%q: ToolSurfaceCanWrite = %v, want %v — %s", tc.backend, tc.dflt, got, tc.canWrite, tc.why)
		}
		if got := isMutatingNodeWithBackend(node, tc.dflt, nil, nil); got != tc.mutating {
			t.Errorf("backend=%q default=%q: isMutatingNode = %v, want %v — the shared-worktree verdicts do not move: %s", tc.backend, tc.dflt, got, tc.mutating, tc.why)
		}
	}
}

// TestThePreRunReadingDoesNotSeeTheRuntimesOwnAppends pins the blind spot the
// two sentences about this predicate name — ToolSurfaceWriteReason's own doc
// comment and the "what it does not read" paragraph of
// docs/agents/bots/untrusted-input.md.
//
// `auto_memory: on` grants claw's file trio and ultracode grants claw's
// unbounded `agent` tool, both over a RESTRICTED `tools:` list, and neither
// resolves in the IR — they come from a launch flag, the workflow, an edge or
// an environment variable. So the pre-run reading answers "" for a node that
// WILL hold a writer, and only the question that holds an executor refuses it.
// A consumer of the predicate inherits that hole; it inherits it VISIBLY, with
// this row, instead of in silence.
//
// The third row is the same hole on the route whose bound depends on the
// NAMES: on codex an append adds no writer, it changes the sandbox MODE, and
// the question that holds an executor is the one that can see it. The two
// readings part there on purpose, and the row is what keeps the parting
// visible rather than silent.
//
// Teaching the pre-run walk to read these fields reddens this test. That is
// the point: the widening is welcome, and it arrives together with the two
// sentences that promise the hole exists and with the catalogue members it
// adds to the class.
func TestThePreRunReadingDoesNotSeeTheRuntimesOwnAppends(t *testing.T) {
	for _, tc := range []struct {
		name  string
		node  *ir.AgentNode
		holds string
		why   string
	}{
		{
			name:  "auto_memory grants write_file over a restricted list",
			node:  agentNode("n", []string{"read_file"}, "claw", func(n *ir.AgentNode) { n.AutoMemory = "on" }),
			holds: "write_file",
			why:   "the question that HOLDS an executor is the one that reads these appends",
		},
		{
			name:  "ultracode grants the unbounded agent subagent tool",
			node:  agentNode("n", []string{"read_file"}, "claw", func(n *ir.AgentNode) { n.ReasoningEffort = "ultracode" }),
			holds: "agent",
			why:   "the question that HOLDS an executor is the one that reads these appends",
		},
		{
			name:  "interaction puts ask_user on a codex list, which buys the workspace-write sandbox",
			node:  agentNode("n", []string{"read_file"}, "codex", func(n *ir.AgentNode) { n.Interaction = ir.InteractionHuman }),
			holds: "ask_user",
			why:   "`ask_user` is not in codex's read-only set, so the list the route RECEIVES buys the workspace-write sandbox, where codex's built-in shell acts on the worktree — a bound read from the NAMES in a list is read from the list the route gets",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The REAL executor answers what the node holds, never a stub: a
			// stub kinder or harsher than the product certifies the test.
			wf := &ir.Workflow{Nodes: map[string]ir.Node{tc.node.ID: tc.node}}
			e := effectiveSurfaceEngine(t, wf)
			held := e.toolSurfaceResolver().EffectiveToolNames(tc.node, false)
			if !slices.Contains(held, tc.holds) {
				t.Fatalf("the node holds %v, without %q: the append this row is about is gone, so the two sentences that name it must go too", held, tc.holds)
			}
			if got := ToolSurfaceWriteReason(tc.node, "", authoredDefaults); got != "" {
				t.Errorf("the pre-run reading answers %q for a node whose %q comes from outside the IR: widening it is welcome, and it comes with the two sentences promising it does not read these fields (ToolSurfaceWriteReason's doc comment, docs/agents/bots/untrusted-input.md) and with the catalogue members the widening adds to the class",
					got, tc.holds)
			}
			if !isMutatingNodeIn(wf, tc.node, "", e.backendResolver(), e.toolSurfaceResolver(), false) {
				t.Errorf("shared-worktree admission admits a node holding %q: %s", tc.holds, tc.why)
			}
		})
	}
}

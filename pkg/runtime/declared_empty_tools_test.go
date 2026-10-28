package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func noEnv(string) string { return "" }

// A `tools: []` node reaches exactly the verdict an undeclared list reaches,
// because it is not a proof that the node holds nothing:
//
//   - on claude_code the bound is `--disallowedTools` over `claudeNativeTools`,
//     a twenty-name declaration of a roster iterion does not own — the same
//     package's `orchestrationTools` still names `TaskOutput` and `Monitor`
//     outside it, and MCP tools are not on it either (`Agent` joined the
//     roster with #1651);
//   - on claw the runtime's own `interaction:` append puts `ask_user` back,
//     and the appends below it `todo_write` and, under `auto_memory:`,
//     `write_file` (C270 warns about exactly that).
//
// The admission is decided once, before the run, over a shared worktree, so
// an empty declaration reaches exactly the pessimistic verdicts an undeclared
// one reaches. Reddens on the mutation that reads `tools: []` as a proof of
// tool-lessness — which would admit N concurrent branches, each with a shell.
// The pair is walked over the node's ROUTES as well as its backends: the
// `fallbacks:` spellings are read by a different arm than the primary, and an
// arm that answers for one list shape and not the other inverts the pair
// exactly where the walk cannot read the route.
func TestADeclaredEmptyToolListReachesTheSameVerdictAsAnUndeclaredOne(t *testing.T) {
	routes := []struct {
		name string
		set  bool
		fb   string
	}{
		{name: "no fallbacks:", set: false},
		{name: "a route that inherits the node's backend", set: true, fb: ""},
		{name: "a route explicitly spelled auto", set: true, fb: "auto"},
		{name: "a route onto claw", set: true, fb: "claw"},
		{name: "a route onto a CLI backend", set: true, fb: "claude_code"},
		{name: "a route onto a backend the list never reaches", set: true, fb: "pi"},
		{name: "a route still spelled as a template", set: true, fb: "{{vars.fb}}"},
		{name: "a route read at an authored default that does not exist", set: true, fb: "${ITERION_TEST_EMPTY_ROUTE}"},
		{name: "the same route unbraced", set: true, fb: "$ITERION_TEST_EMPTY_ROUTE"},
		{name: "a route whose authored default names a backend", set: true, fb: "${ITERION_TEST_EMPTY_ROUTE:-pi}"},
	}
	node := func(backend string, tools []string, r struct {
		name string
		set  bool
		fb   string
	}) *ir.JudgeNode {
		n := &ir.JudgeNode{
			BaseNode:  ir.BaseNode{ID: "j"},
			Tools:     tools,
			LLMFields: ir.LLMFields{Backend: backend},
		}
		if r.set {
			n.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: r.fb}}
		}
		return n
	}
	for _, backend := range []string{"claw", "claude_code", "codex", "pi", "kimi", "grok"} {
		for _, r := range routes {
			declared := node(backend, []string{}, r)
			undeclared := node(backend, nil, r)
			got, want := ToolSurfaceCanWrite(declared, "", noEnv), ToolSurfaceCanWrite(undeclared, "", noEnv)
			if got != want {
				t.Errorf("backend %q with %s: `tools: []` canWrite = %v but an undeclared list = %v — the empty declaration must not buy an admission no backend can honour, and the absence of one must not buy the admission the empty list is refused", backend, r.name, got, want)
			}
		}
	}
}

// The neighbouring readings must not move either: this change touches what a
// tool list MEANS, and the scheduler's existing verdicts are not part of it.
func TestToolSurfaceVerdictsAreUnchangedByTheDeclarationOfEmptiness(t *testing.T) {
	for _, tc := range []struct {
		name     string
		node     *ir.JudgeNode
		canWrite bool
	}{
		{"undeclared on claw", &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "j"}, LLMFields: ir.LLMFields{Backend: "claw"}}, false},
		{"undeclared on claude_code", &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "j"}, LLMFields: ir.LLMFields{Backend: "claude_code"}}, true},
		// Not because of the names, but because of the ROUTE: claude_code takes
		// the list as `--disallowedTools` over a roster iterion does not own.
		{"read-only names on claude_code", &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "j"}, Tools: []string{"read_file"}, LLMFields: ir.LLMFields{Backend: "claude_code"}}, true},
		{"read-only names on claw", &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "j"}, Tools: []string{"read_file"}, LLMFields: ir.LLMFields{Backend: "claw"}}, false},
		{"a shell on claw", &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "j"}, Tools: []string{"bash"}, LLMFields: ir.LLMFields{Backend: "claw"}}, true},
		{"claw with a CLI route", &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "j"}, Tools: []string{"read_file"}, LLMFields: ir.LLMFields{Backend: "claw"}, Fallbacks: []ir.Fallback{{Name: "fb", Backend: "claude_code"}}}, true},
	} {
		if got := ToolSurfaceCanWrite(tc.node, "", noEnv); got != tc.canWrite {
			t.Errorf("%s: canWrite = %v, want %v", tc.name, got, tc.canWrite)
		}
	}
}

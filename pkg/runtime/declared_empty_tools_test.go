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
//     a hardcoded 14-name enumeration of a roster iterion does not own — the
//     same package's `orchestrationTools` names `Agent`, `TaskOutput` and
//     `Monitor` outside it, and MCP tools are not on it either;
//   - on claw the runtime's own `interaction:` append puts `ask_user` back,
//     and the appends below it `todo_write` and, under `auto_memory:`,
//     `write_file` (C270 warns about exactly that).
//
// The admission is decided once, before the run, over a shared worktree, so
// an empty declaration reaches exactly the pessimistic verdicts an undeclared
// one reaches. Reddens on the mutation that reads `tools: []` as a proof of
// tool-lessness — which would admit N concurrent branches, each with a shell.
func TestADeclaredEmptyToolListReachesTheSameVerdictAsAnUndeclaredOne(t *testing.T) {
	for _, backend := range []string{"claw", "claude_code", "codex", "pi", "kimi", "grok"} {
		declared := &ir.JudgeNode{
			BaseNode:  ir.BaseNode{ID: "j"},
			Tools:     []string{},
			LLMFields: ir.LLMFields{Backend: backend},
		}
		undeclared := &ir.JudgeNode{
			BaseNode:  ir.BaseNode{ID: "j"},
			LLMFields: ir.LLMFields{Backend: backend},
		}
		got, want := ToolSurfaceCanWrite(declared, "", noEnv), ToolSurfaceCanWrite(undeclared, "", noEnv)
		if got != want {
			t.Errorf("backend %q: `tools: []` canWrite = %v but an undeclared list = %v — the empty declaration must not buy an admission no backend can honour", backend, got, want)
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
		{"read-only names on claude_code", &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "j"}, Tools: []string{"read_file"}, LLMFields: ir.LLMFields{Backend: "claude_code"}}, false},
		{"a shell on claw", &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "j"}, Tools: []string{"bash"}, LLMFields: ir.LLMFields{Backend: "claw"}}, true},
		{"claw with a CLI route", &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "j"}, Tools: []string{"read_file"}, LLMFields: ir.LLMFields{Backend: "claw"}, Fallbacks: []ir.Fallback{{Name: "fb", Backend: "claude_code"}}}, true},
	} {
		if got := ToolSurfaceCanWrite(tc.node, "", noEnv); got != tc.canWrite {
			t.Errorf("%s: canWrite = %v, want %v", tc.name, got, tc.canWrite)
		}
	}
}

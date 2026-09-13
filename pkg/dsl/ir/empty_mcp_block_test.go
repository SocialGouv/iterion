package ir

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// An `mcp:` block that only DISABLES a server is wiring all the same: the
// author has proven the ambient set exists (inherit and autoload default
// to true when unset), so a bare tool name may be one of its servers' and
// C135 stays the warning it was — not the error a bare block draws.
func TestMCPBlockThatOnlyDisablesAServerIsWiring(t *testing.T) {
	for name, src := range map[string]string{
		"on the node":     "agent a:\n  backend: \"claw\"\n  model: \"m\"\n  tools: [list_files]\n  mcp:\n    disable: [noisy]\n\nworkflow w:\n  entry: a\n  a -> done\n",
		"on the workflow": "agent a:\n  backend: \"claw\"\n  model: \"m\"\n  tools: [list_files]\n\nworkflow w:\n  entry: a\n  mcp:\n    disable: [noisy]\n  a -> done\n",
	} {
		pr := parser.Parse("x.bot", src)
		for _, d := range pr.Diagnostics {
			t.Fatalf("%s: parse: %s", name, d.Error())
		}
		cr := Compile(pr.File)
		var seen bool
		for _, d := range cr.Diagnostics {
			if d.Code == DiagUnknownTool {
				seen = true
				if d.Severity != SeverityWarning {
					t.Fatalf("%s: C135 is %v; a block that disables an ambient server has shown the ambient set exists, so the name may be a server's tool", name, d.Severity)
				}
			}
		}
		if !seen {
			t.Fatalf("%s: no C135 at all: %v", name, cr.Diagnostics)
		}
	}
}

// An `mcp:` block that wires nothing — the bare header the studio saves
// the moment the block is created — is not MCP wiring: a tool name claw
// cannot resolve stays the error it is without the block (C135), instead
// of softening to the warning reserved for a bot that visibly wires
// servers whose tools the compiler cannot see.
func TestBareMCPBlockDoesNotDisarmTheToolNameError(t *testing.T) {
	without := "agent a:\n  backend: \"claw\"\n  model: \"m\"\n  tools: [list_files]\n\nworkflow w:\n  entry: a\n  a -> done\n"
	with := "agent a:\n  backend: \"claw\"\n  model: \"m\"\n  tools: [list_files]\n  mcp:\n\nworkflow w:\n  entry: a\n  a -> done\n"
	for name, src := range map[string]string{"without mcp block": without, "with a bare mcp block": with} {
		pr := parser.Parse("x.bot", src)
		for _, d := range pr.Diagnostics {
			t.Fatalf("%s: parse: %s", name, d.Error())
		}
		cr := Compile(pr.File)
		var severity Severity
		var seen bool
		for _, d := range cr.Diagnostics {
			if d.Code == DiagUnknownTool {
				severity, seen = d.Severity, true
			}
		}
		if !seen {
			t.Fatalf("%s: no C135 for a tool claw cannot resolve: %v", name, cr.Diagnostics)
		}
		if severity != SeverityError {
			t.Fatalf("%s: C135 is %v; a block that wires nothing must not soften the error", name, severity)
		}
	}
}

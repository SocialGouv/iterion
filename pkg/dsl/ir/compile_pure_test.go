package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Compile must not mutate its input: group expansion used to append the
// expanded nodes and edges onto the caller's *ast.File, so a caller that
// compiled a file and then serialised or compiled it again shipped a program
// with every `use` expanded twice (duplicate node ids, C041). The JSON
// transport carries groups and uses now, which turned that side effect into
// a wire defect.
func TestCompileDoesNotMutateItsInput(t *testing.T) {
	lines := []string{
		"schema pout:",
		"  ok: bool",
		"",
		"group gate_block(label):",
		"  tool gate:",
		"    command: `printf '{\"ok\":true}'`",
		"    output: pout",
		"  tool check:",
		"    command: `printf '{\"ok\":true}'`",
		"    output: pout",
		"  gate -> check",
		"",
		"use gate_block as r1 with { label: \"A\" }",
		"use gate_block as r2 with { label: \"B\" }",
		"",
		"workflow w:",
		"  entry: r1.gate",
		"  r1.check -> r2.gate",
		"  r2.check -> done",
	}
	pr := parser.Parse("pure.bot", strings.Join(lines, "\n")+"\n")
	for _, d := range pr.Diagnostics {
		t.Fatalf("unexpected parse diagnostic: %s", d.Error())
	}
	tools, edges := len(pr.File.Tools), len(pr.File.Workflows[0].Edges)

	first := Compile(pr.File)
	if first.HasErrors() {
		t.Fatalf("first compile: %v", first.Diagnostics)
	}
	if got := len(pr.File.Tools); got != tools {
		t.Errorf("Compile appended %d expanded tool(s) onto the caller's file", got-tools)
	}
	if got := len(pr.File.Workflows[0].Edges); got != edges {
		t.Errorf("Compile appended %d expanded edge(s) onto the caller's workflow", got-edges)
	}

	second := Compile(pr.File)
	if second.HasErrors() {
		t.Errorf("a second compile of the same file fails: %v", second.Diagnostics)
	}
	if len(second.Workflow.Nodes) != len(first.Workflow.Nodes) {
		t.Errorf("second compile has %d nodes, first had %d", len(second.Workflow.Nodes), len(first.Workflow.Nodes))
	}
}

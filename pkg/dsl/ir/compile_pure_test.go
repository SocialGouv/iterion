package ir

import (
	"reflect"
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
	src := strings.Join(lines, "\n") + "\n"
	pr := parser.Parse("pure.bot", src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("unexpected parse diagnostic: %s", d.Error())
	}
	// The oracle is a second, untouched parse of the same source: after
	// Compile, the compiled input must still equal it field for field — not
	// only in the lengths of the slices the expansion appends to (a mutant
	// that scaled a shared resource capacity passed that weaker check).
	fresh := parser.Parse("pure.bot", src).File

	first := Compile(pr.File)
	if first.HasErrors() {
		t.Fatalf("first compile: %v", first.Diagnostics)
	}
	if !reflect.DeepEqual(pr.File, fresh) {
		t.Errorf("Compile changed its input: %d tools (was %d), %d edges (was %d)",
			len(pr.File.Tools), len(fresh.Tools), len(pr.File.Workflows[0].Edges), len(fresh.Workflows[0].Edges))
	}

	second := Compile(pr.File)
	if second.HasErrors() {
		t.Errorf("a second compile of the same file fails: %v", second.Diagnostics)
	}
	if len(second.Workflow.Nodes) != len(first.Workflow.Nodes) {
		t.Errorf("second compile has %d nodes, first had %d", len(second.Workflow.Nodes), len(first.Workflow.Nodes))
	}
}

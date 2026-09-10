package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// An edge inside a group body is cloned for every `use`; an edge-attributed
// finding on the clone must point at the body edge the author edits — the
// expanded edges enter the workflow before positions are attached, so they
// are positioned like any other edge. (A node-attributed finding such as
// C012 points at the node's declaration, in a group as at the top level.)
func TestExpandedEdgeFindingsPointAtTheGroupBodyLine(t *testing.T) {
	lines := []string{
		/* 1 */ "schema out:",
		/* 2 */ "  ok: bool",
		/* 3 */ "",
		/* 4 */ "group blk:",
		/* 5 */ "  tool a:",
		/* 6 */ "    command: `true`",
		/* 7 */ "    output: out",
		/* 8 */ "  tool b:",
		/* 9 */ "    command: `true`",
		/* 10 */ "    output: out",
		/* 11 */ "  a -> b when nope",
		/* 12 */ "  a -> b else",
		/* 13 */ "",
		/* 14 */ "use blk as g1",
		/* 15 */ "",
		/* 16 */ "workflow w:",
		/* 17 */ "  entry: g1.a",
		/* 18 */ "  g1.b -> done",
	}
	pr := parser.Parse("grp.bot", strings.Join(lines, "\n")+"\n")
	for _, d := range pr.Diagnostics {
		t.Fatalf("unexpected parse diagnostic: %s", d.Error())
	}
	res := Compile(pr.File)
	var got []int
	for _, d := range res.Diagnostics {
		if d.Code == DiagConditionFieldNotFound {
			got = append(got, d.Line)
		}
	}
	if len(got) != 1 || got[0] != 11 {
		t.Errorf("C014 on the expanded edge at lines %v, want [11] (the group body edge)\n%v", got, res.Diagnostics)
	}
}

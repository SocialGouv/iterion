package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A "duplicate" finding must point at the REPEAT — the line the author
// deletes — not at the first occurrence, which the node id alone resolves to
// and which is the declaration they keep.
func TestDuplicateFindingsPointAtTheRepeat(t *testing.T) {
	lines := []string{
		/* 1 */ "schema out:",
		/* 2 */ "  ok: bool",
		/* 3 */ "",
		/* 4 */ "agent b:",
		/* 5 */ "  model: \"m\"",
		/* 6 */ "  output: out",
		/* 7 */ "",
		/* 8 */ "router r:",
		/* 9 */ "  mode: fan_out_all",
		/* 10 */ "",
		/* 11 */ "agent b:",
		/* 12 */ "  model: \"m\"",
		/* 13 */ "  output: out",
		/* 14 */ "",
		/* 15 */ "agent c:",
		/* 16 */ "  model: \"m\"",
		/* 17 */ "  output: out",
		/* 18 */ "  await: wait_all",
		/* 19 */ "",
		/* 20 */ "workflow w:",
		/* 21 */ "  entry: r",
		/* 22 */ "  r -> b",
		/* 23 */ "  r -> c",
		/* 24 */ "  r -> b",
		/* 25 */ "  b -> c",
		/* 26 */ "  c -> done",
	}
	pr := parser.Parse("dup.bot", strings.Join(lines, "\n")+"\n")
	for _, d := range pr.Diagnostics {
		t.Fatalf("unexpected parse diagnostic: %s", d.Error())
	}
	res := Compile(pr.File)
	at := map[DiagCode][]int{}
	for _, d := range res.Diagnostics {
		at[d.Code] = append(at[d.Code], d.Line)
	}
	if got := at[DiagDuplicateNodeID]; len(got) != 1 || got[0] != 11 {
		t.Errorf("C041 at lines %v, want [11] (the redeclaration)\n%v", got, res.Diagnostics)
	}
	if got := at[DiagDuplicateFanOutTarget]; len(got) != 1 || got[0] != 24 {
		t.Errorf("C249 at lines %v, want [24] (the repeated edge)\n%v", got, res.Diagnostics)
	}
}

// Two `use` blocks with one prefix expand to the same node ids. The finding
// points at the repeated `use` line — the line to change — not at the group
// body both instances are copied from, and it is reported once.
func TestDuplicateUsePrefixPointsAtTheRepeatedUse(t *testing.T) {
	lines := []string{
		/* 1 */ "schema pout:",
		/* 2 */ "  ok: bool",
		/* 3 */ "",
		/* 4 */ "group gate_block(label):",
		/* 5 */ "  tool gate:",
		/* 6 */ "    command: `printf '{\"ok\":true}'`",
		/* 7 */ "    output: pout",
		/* 8 */ "",
		/* 9 */ "use gate_block as r1 with { label: \"A\" }",
		/* 10 */ "use gate_block as r1 with { label: \"B\" }",
		/* 11 */ "",
		/* 12 */ "workflow w:",
		/* 13 */ "  entry: r1.gate",
		/* 14 */ "  r1.gate -> done",
	}
	pr := parser.Parse("use.bot", strings.Join(lines, "\n")+"\n")
	for _, d := range pr.Diagnostics {
		t.Fatalf("unexpected parse diagnostic: %s", d.Error())
	}
	res := Compile(pr.File)
	var got []int
	for _, d := range res.Diagnostics {
		if d.Code == DiagDuplicateNodeID {
			got = append(got, d.Line)
		}
	}
	if len(got) != 1 || got[0] != 10 {
		t.Errorf("C041 at lines %v, want [10] (the repeated use)\n%v", got, res.Diagnostics)
	}
}

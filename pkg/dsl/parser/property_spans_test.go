package parser

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// spanAt is a span as four numbers — start line and column, end line and
// column — for a readable assertion.
func spanAt(sp ast.Span) [4]int {
	return [4]int{sp.Start.Line, sp.Start.Column, sp.End.Line, sp.End.Column}
}

// TestEveryPropertyLineKeepsItsSpan: each property line of a declaration is
// recorded from its name to the end of its value — a block property to the
// end of its body, a multi-line value to the end of its last line — a
// trailing comment left out, a property written twice keeping the last
// line's span (the value the parser keeps), and an edge recorded as none.
func TestEveryPropertyLineKeepsItsSpan(t *testing.T) {
	src := "agent a:\n" +
		"  model: \"gpt\"\n" + // 2
		"  tools: [a, b]  ## note\n" + // 3
		"  mcp:\n" + // 4
		"    servers: [x]\n" + // 5
		"  system: |\n" + // 6
		"    line one\n" + // 7
		"    line two\n" + // 8
		"  model: \"again\"\n" + // 9
		"\n" +
		"workflow w:\n" + // 11
		"  entry: a\n" + // 12
		"  a -> done\n" + // 13
		"  compress: on\n" // 14
	res, err := Parse("spans.bot", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diagnostics) > 0 {
		t.Fatalf("the probe drew %v", res.Diagnostics)
	}
	agent := res.File.Agents[0].Properties
	for name, want := range map[string][4]int{
		"model":  {9, 3, 9, 17},
		"tools":  {3, 3, 3, 16},
		"mcp":    {4, 3, 5, 17},
		"system": {6, 3, 8, 13},
	} {
		sp, ok := agent.Lookup(name)
		if !ok {
			t.Errorf("agent: %s has no span", name)
			continue
		}
		if got := spanAt(sp); got != want {
			t.Errorf("agent %s: span %v, want %v", name, got, want)
		}
		if sp.Start.File != "spans.bot" || sp.End.File != "spans.bot" {
			t.Errorf("agent %s: span files %q / %q, want the source's", name, sp.Start.File, sp.End.File)
		}
	}
	if len(agent) != 4 {
		t.Errorf("agent keeps %d entries, want 4 (one per property name): %+v", len(agent), agent)
	}
	wf := res.File.Workflows[0].Properties
	for name, want := range map[string][4]int{
		"entry":    {12, 3, 12, 11},
		"compress": {14, 3, 14, 15},
	} {
		sp, ok := wf.Lookup(name)
		if !ok {
			t.Errorf("workflow: %s has no span", name)
			continue
		}
		if got := spanAt(sp); got != want {
			t.Errorf("workflow %s: span %v, want %v", name, got, want)
		}
	}
	if _, ok := wf.Lookup("a"); ok || len(wf) != 2 {
		t.Errorf("workflow keeps %d entries (%+v), want entry and compress alone: an edge is no property", len(wf), wf)
	}
}

// TestEveryNodeKindKeepsItsPropertySpans: the one property line every node
// kind accepts is recorded for every kind, where its name starts.
func TestEveryNodeKindKeepsItsPropertySpans(t *testing.T) {
	src := "agent ag:\n  description: \"d\"\n" +
		"judge ju:\n  description: \"d\"\n" +
		"router ro:\n  description: \"d\"\n" +
		"human hu:\n  description: \"d\"\n" +
		"tool to:\n  description: \"d\"\n" +
		"compute co:\n  description: \"d\"\n" +
		"subbot su:\n  description: \"d\"\n" +
		"emit em:\n  description: \"d\"\n" +
		"wait wa:\n  description: \"d\"\n" +
		"await_answers aw:\n  description: \"d\"\n" +
		"fail fa:\n  description: \"d\"\n"
	res, err := Parse("kinds.bot", src)
	if err != nil {
		t.Fatal(err)
	}
	f := res.File
	tables := map[string]ast.PropertySpans{
		"agent": f.Agents[0].Properties, "judge": f.Judges[0].Properties, "router": f.Routers[0].Properties,
		"human": f.Humans[0].Properties, "tool": f.Tools[0].Properties, "compute": f.Computes[0].Properties,
		"subbot": f.Subbots[0].Properties, "emit": f.Emits[0].Properties, "wait": f.Waits[0].Properties,
		"await_answers": f.AwaitAnswers[0].Properties, "fail": f.Fails[0].Properties,
	}
	for kind, table := range tables {
		sp, ok := table.Lookup("description")
		if !ok {
			t.Errorf("%s: description has no span", kind)
			continue
		}
		if sp.Start.Column != 3 || sp.End.Line != sp.Start.Line || sp.End.Column != 3+len(`description: "d"`) {
			t.Errorf("%s: description span %v", kind, spanAt(sp))
		}
	}
}

// TestARefusedPropertyIsNotRecorded: a name the declaration refuses (E012)
// is no property of it and takes no entry — so a file of strays leaves the
// table as small as the registry, and Set, linear in the table, stays linear
// in the file. A hundred thousand strays parse in the time the lexer takes.
func TestARefusedPropertyIsNotRecorded(t *testing.T) {
	res, err := Parse("strays.bot", "agent a:\n  model: \"m\"\n  zz_bogus: 1\n  tools: [x]\n")
	if err != nil {
		t.Fatal(err)
	}
	table := res.File.Agents[0].Properties
	if _, ok := table.Lookup("zz_bogus"); ok || len(table) != 2 {
		t.Fatalf("the refused property is recorded: %+v", table)
	}
	// The race detector multiplies the constant, not the exponent: a tenth
	// of the strays under it, the same budget, the same linear claim.
	strays := 100000
	if raceEnabled {
		strays = 10000
	}
	var b strings.Builder
	b.WriteString("agent a:\n  model: \"m\"\n")
	for i := 0; i < strays; i++ {
		fmt.Fprintf(&b, "  zz_stray_%d: 1\n", i)
	}
	t.Setenv(ReadBudgetEnv, "0")
	start := time.Now()
	res, err = Parse("strays.bot", b.String())
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("%d strays took %s to parse: the table grows with them", strays, took)
	}
	if n := len(res.File.Agents[0].Properties); n != 1 {
		t.Fatalf("the table holds %d entries for one accepted property", n)
	}
}

// TestARefusedNameIsRecordedWhereAnotherKindAcceptsIt: the line
// unknownProperty refused is left out of ITS declaration's table only — the
// same name accepted by the next declaration is recorded there.
func TestARefusedNameIsRecordedWhereAnotherKindAcceptsIt(t *testing.T) {
	res, err := Parse("kinds.bot", "emit e:\n  event: \"x\"\n  timeout: \"1s\"\nwait w:\n  event: \"x\"\n  timeout: \"1s\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.File.Emits[0].Properties.Lookup("timeout"); ok {
		t.Errorf("emit records the timeout it refused: %+v", res.File.Emits[0].Properties)
	}
	if sp, ok := res.File.Waits[0].Properties.Lookup("timeout"); !ok || sp.Start.Line != 6 {
		t.Errorf("wait does not record the timeout it accepts: %+v", res.File.Waits[0].Properties)
	}
}

// TestASpanClosesOnTheLexersDiagnosis: a value the lexer refuses leaves a
// diagnosis token standing on the refused rune, and the property's span
// closes past that rune — not on the keyword, not past the diagnosis text.
func TestASpanClosesOnTheLexersDiagnosis(t *testing.T) {
	res, err := Parse("lex.bot", "agent a:\n  timeout: ?\n")
	if err != nil {
		t.Fatal(err)
	}
	sp, ok := res.File.Agents[0].Properties.Lookup("timeout")
	if !ok {
		t.Fatal("timeout has no span")
	}
	if got := spanAt(sp); got != [4]int{2, 3, 2, 13} {
		t.Errorf("timeout span %v, want it to close past the refused ? (2:13)", got)
	}
}

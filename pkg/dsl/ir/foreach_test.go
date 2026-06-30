package ir

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

const foreachSrc = `
schema empty:
  ok: bool

tool start:
  command: "true"
  output: empty

tool proc:
  command: "echo {{each.scan.item}}"
  output: empty

workflow w:
  entry: start

  start -> proc
  proc -> proc as foreach scan(item in "{{outputs.start.items}}")
  proc -> done
`

// TestCompileForeach checks the foreach clause compiles into a Foreach
// definition + an edge carrying ForeachName.
func TestCompileForeach(t *testing.T) {
	w := mustCompile(t, foreachSrc)
	fe, ok := w.Foreaches["scan"]
	if !ok {
		t.Fatalf("foreach 'scan' not found; foreaches=%v", w.Foreaches)
	}
	if fe.Item != "item" || fe.CollectionRaw != "{{outputs.start.items}}" || len(fe.CollectionRefs) != 1 {
		t.Fatalf("foreach fields: item=%q coll=%q refs=%d", fe.Item, fe.CollectionRaw, len(fe.CollectionRefs))
	}
	found := false
	for _, e := range w.Edges {
		if e.From == "proc" && e.To == "proc" && e.ForeachName == "scan" {
			found = true
		}
	}
	if !found {
		t.Fatal("no edge carries ForeachName 'scan'")
	}
}

// TestForeachConflictsLoop asserts C118 fires if an edge combines foreach+loop.
// (The parser keeps only the first `as` clause, so this is constructed at the
// AST level to exercise the compiler guard.)
func TestForeachConflictsLoop(t *testing.T) {
	f := parseFile(t, foreachSrc)
	// Graft a Loop clause onto the foreach edge to simulate both being present.
	for _, wf := range f.Workflows {
		for _, e := range wf.Edges {
			if e.Foreach != nil {
				e.Loop = &ast.LoopClause{Name: "x", MaxIterations: 3}
			}
		}
	}
	r := Compile(f)
	if !hasDiag(r.Diagnostics, DiagForeachConflictsLoop) {
		t.Fatalf("expected C118 (DiagForeachConflictsLoop), got: %v", r.Diagnostics)
	}
}

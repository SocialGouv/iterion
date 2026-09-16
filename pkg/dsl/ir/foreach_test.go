package ir

import (
	"strings"
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

// TestEdgesMaySHAREAForeachButNotDisagreeAboutIt.
//
// Registration keeps the FIRST declaration of a name, so two edges that
// disagree about what a foreach iterates used to compile clean and the second
// walked the first one's collection — no error, no warning, wrong data. The
// loop arm beside it refuses the same shape, for the reason it states: the
// runtime resolution would be ambiguous.
//
// Sharing stays legal: one cursor re-entered from several edges is the same
// iteration, exactly as edges may share a loop.
func TestEdgesMaySHAREAForeachButNotDisagreeAboutIt(t *testing.T) {
	const src = `schema empty:
  ok: bool
tool start:
  command: "true"
  output: empty
tool proc:
  command: "echo {{each.scan.item}}"
  output: empty
tool other:
  command: "true"
  output: empty
workflow w:
  entry: start
  start -> proc
  proc -> proc as foreach scan(item in "{{outputs.start.items}}")
  proc -> other
  other -> other as foreach scan(ITEM in "COLLECTION")
  other -> done
`
	for _, tc := range []struct {
		name, item, collection string
		refused                bool
	}{
		{"same declaration shares one cursor", "item", "{{outputs.start.items}}", false},
		{"a different collection", "item", "{{outputs.proc.items}}", true},
		{"a different element binding", "row", "{{outputs.start.items}}", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(src, "ITEM", tc.item, 1)
			body = strings.Replace(body, "COLLECTION", tc.collection, 1)
			r := Compile(parseFile(t, body))
			if got := hasDiag(r.Diagnostics, DiagDuplicateForeach); got != tc.refused {
				t.Fatalf("C269 present = %v, want %v; diagnostics=%v", got, tc.refused, r.Diagnostics)
			}
			if tc.refused {
				return
			}
			// Shared: ONE definition, and both edges point at it.
			if n := len(r.Workflow.Foreaches); n != 1 {
				t.Fatalf("foreaches = %d, want the one both edges declare", n)
			}
			carriers := 0
			for _, e := range r.Workflow.Edges {
				if e.ForeachName == "scan" {
					carriers++
				}
			}
			if carriers != 2 {
				t.Fatalf("edges carrying the shared foreach = %d, want 2", carriers)
			}
		})
	}
}

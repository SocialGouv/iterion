package ir

import "testing"

const groupSrc = `
schema empty:
  ok: bool

schema verdict:
  approved: bool

group review_block(target, max_fix):
  judge check:
    model: "test-model"
    output: verdict
  tool fix:
    command: "echo fixing {{params.target}}"
    output: empty
  check -> fix when not approved
  fix -> check as fix_loop("{{params.max_fix}}")

tool analyze:
  command: "true"
  output: empty

use review_block as r1 with { target: "alpha", max_fix: "3" }

workflow w:
  entry: analyze

  analyze -> r1.check
  r1.check -> done when approved
`

// TestExpandGroup checks a `use` instantiation produces prefixed nodes, rewires
// the internal edges, and substitutes {{params.X}}.
func TestExpandGroup(t *testing.T) {
	w := mustCompile(t, groupSrc)

	// Prefixed nodes exist.
	for _, id := range []string{"r1.check", "r1.fix", "analyze"} {
		if _, ok := w.Nodes[id]; !ok {
			t.Fatalf("expected node %q after group expansion; nodes=%v", id, nodeIDs(w))
		}
	}

	// Param substitution landed in the cloned tool command.
	fix, ok := w.Nodes["r1.fix"].(*ToolNode)
	if !ok {
		t.Fatalf("r1.fix is not a ToolNode: %T", w.Nodes["r1.fix"])
	}
	if fix.Command != "echo fixing alpha" {
		t.Fatalf("param not substituted in command: %q", fix.Command)
	}

	// Internal edge rewired to prefixed endpoints.
	foundInternal := false
	for _, e := range w.Edges {
		if e.From == "r1.check" && e.To == "r1.fix" {
			foundInternal = true
		}
	}
	if !foundInternal {
		t.Fatalf("internal group edge r1.check->r1.fix not found; edges=%v", edgePairs(w))
	}

	// Loop cap template substituted: the fix->check loop carries max 3.
	lp, ok := w.Loops["fix_loop"]
	if !ok {
		t.Fatalf("loop fix_loop not found; loops=%v", w.Loops)
	}
	if lp.MaxIterationsExpr != "3" && lp.MaxIterations != 3 {
		t.Fatalf("loop cap not substituted: expr=%q lit=%d", lp.MaxIterationsExpr, lp.MaxIterations)
	}
}

// TestUseUnknownGroup asserts C116 fires for a use of an undeclared group.
func TestUseUnknownGroup(t *testing.T) {
	src := `
schema empty:
  ok: bool

tool a:
  command: "true"
  output: empty

use ghost as g1

workflow w:
  entry: a
  a -> done
`
	if !hasDiag(compileFile(t, src).Diagnostics, DiagUseUnknownGroup) {
		t.Fatal("expected C116 (DiagUseUnknownGroup)")
	}
}

// TestUseParamMismatch asserts C117 fires for an unknown / missing param.
func TestUseParamMismatch(t *testing.T) {
	src := `
schema empty:
  ok: bool

group blk(needed):
  tool t:
    command: "true"
    output: empty

tool a:
  command: "true"
  output: empty

use blk as b1 with { wrong: "x" }

workflow w:
  entry: a
  a -> b1.t
  b1.t -> done
`
	if !hasDiag(compileFile(t, src).Diagnostics, DiagUseParamMismatch) {
		t.Fatal("expected C117 (DiagUseParamMismatch)")
	}
}

func nodeIDs(w *Workflow) []string {
	var ids []string
	for id := range w.Nodes {
		ids = append(ids, id)
	}
	return ids
}

func edgePairs(w *Workflow) []string {
	var ps []string
	for _, e := range w.Edges {
		ps = append(ps, e.From+"->"+e.To)
	}
	return ps
}

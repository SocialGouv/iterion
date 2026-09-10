package ir

import (
	"testing"
)

// cascadeGroupSrc binds `a` to text that itself reads like a reference to
// `b`. Only `{{params.a}}` appears in the group body, so a single-pass
// expansion can only ever produce the literal `{{params.b}}` — a second
// pass over the substituted text is what turns it into `X`.
const cascadeGroupSrc = `
schema empty:
  ok: bool

group blk(a, b):
  tool t:
    command: "echo {{params.a}}"
    output: empty

tool start:
  command: "true"
  output: empty

use blk as g1 with { a: "{{params.b}}", b: "X" }

workflow w:
  entry: start
  start -> g1.t
  g1.t -> done
`

// expandedCommand compiles src and returns the command of the expanded
// group node `name`, reading the AST the compiler instantiated (the
// substitution's own output, independent of any downstream diagnostic the
// residual text may raise).
// expandedCommand reads the expanded tool from the COMPILED workflow: the
// compiler does not touch the caller's file, so the expansion is only
// observable in its output.
func expandedCommand(t *testing.T, src, name string) string {
	t.Helper()
	file := parseFile(t, src)
	cr := Compile(file)
	if cr.Workflow == nil {
		t.Fatalf("compile failed: %v", cr.Diagnostics)
	}
	if tn, ok := cr.Workflow.Nodes[name].(*ToolNode); ok {
		return tn.Command
	}
	var names []string
	for id := range cr.Workflow.Nodes {
		names = append(names, id)
	}
	t.Fatalf("expanded tool %q not found; nodes=%v", name, names)
	return ""
}

// TestGroupParamSubstitutionIsSinglePass asserts a bound value is never
// itself scanned for `{{params.*}}`: substituting `{{params.a}}` yields the
// bound text verbatim, even when that text reads like another parameter
// reference.
func TestGroupParamSubstitutionIsSinglePass(t *testing.T) {
	got := expandedCommand(t, cascadeGroupSrc, "g1.t")
	if want := "echo {{params.b}}"; got != want {
		t.Fatalf("bound value was re-expanded: got %q, want %q", got, want)
	}
}

// TestGroupParamSubstitutionIsDeterministic asserts the expansion of one
// source is byte-identical across repeated compiles — the bind map's
// iteration order must not reach the output.
func TestGroupParamSubstitutionIsDeterministic(t *testing.T) {
	first := expandedCommand(t, cascadeGroupSrc, "g1.t")
	for i := 1; i < 50; i++ {
		if got := expandedCommand(t, cascadeGroupSrc, "g1.t"); got != first {
			t.Fatalf("compile %d differs: got %q, first compile got %q", i, got, first)
		}
	}
}

// TestGroupParamUnboundStaysLiteral asserts a `{{params.x}}` with no
// binding survives verbatim, so the reference validator can report it.
func TestGroupParamUnboundStaysLiteral(t *testing.T) {
	src := `
schema empty:
  ok: bool

group blk(a):
  tool t:
    command: "echo {{params.a}} {{params.ghost}}"
    output: empty

tool start:
  command: "true"
  output: empty

use blk as g1 with { a: "alpha" }

workflow w:
  entry: start
  start -> g1.t
  g1.t -> done
`
	got := expandedCommand(t, src, "g1.t")
	if want := "echo alpha {{params.ghost}}"; got != want {
		t.Fatalf("unbound param not left literal: got %q, want %q", got, want)
	}
}

// TestGroupParamSubstitutionKeepsUnterminatedTail asserts a malformed
// `{{` with no closing fence is copied through unchanged rather than
// truncating the rest of the value.
func TestGroupParamSubstitutionKeepsUnterminatedTail(t *testing.T) {
	src := `
schema empty:
  ok: bool

group blk(a):
  tool t:
    command: "echo {{params.a}} tail {{ oops"
    output: empty

tool start:
  command: "true"
  output: empty

use blk as g1 with { a: "alpha" }

workflow w:
  entry: start
  start -> g1.t
  g1.t -> done
`
	got := expandedCommand(t, src, "g1.t")
	if want := "echo alpha tail {{ oops"; got != want {
		t.Fatalf("unterminated template tail lost: got %q, want %q", got, want)
	}
}

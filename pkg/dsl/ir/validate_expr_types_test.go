package ir

import (
	"fmt"
	"testing"
)

// exprTestSchema declares one field per type the checks reason about, so a
// single source node can host every when-expression case below.
const exprTestSchema = `  family: string [enum: "claude", "gpt"]
  count: int
  tags: string[]
  approved: bool
  name: string
  blob: json`

// whenExprTypeSrc builds a reviewer whose output schema is exprTestSchema and an
// edge `reviewer -> done when "<expr>"` (plus an unconditional fallback). The
// when-expression resolves against the reviewer's OUTPUT, exactly as the
// runtime exposes it, so bare identifiers (family, count, …) resolve.
func whenExprTypeSrc(expr string) string {
	return fmt.Sprintf(`
schema rev:
%s

prompt sys:
  hi

agent reviewer:
  backend: "claw"
  model: "anthropic/claude-sonnet-4-6"
  output: rev
  system: sys

workflow w:
  entry: reviewer
  reviewer -> done when "%s"
  reviewer -> fail
`, exprTestSchema, expr)
}

func countCode(r *CompileResult, code DiagCode) int {
	n := 0
	for _, d := range r.Diagnostics {
		if d.Code == code {
			n++
		}
	}
	return n
}

func TestC121_EnumLiteralMismatch(t *testing.T) {
	cases := []struct {
		expr string
		want int
	}{
		{"family == 'claude'", 0},         // valid member
		{"family == 'gpt'", 0},            // valid member
		{"family == 'cluade'", 1},         // typo → never matches
		{"family != 'xxx'", 1},            // != a non-member is always-true (typo)
		{"family != 'gpt'", 0},            // valid member
		{"name == 'whatever'", 0},         // name has no enum → not checked
		{"blob == 'whatever'", 0},         // json → no opinion
		{"!approved && family == 'x'", 1}, // nested inside a bigger expression
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			r := compileFile(t, whenExprTypeSrc(tc.expr))
			if got := countCode(r, DiagEnumLiteralMismatch); got != tc.want {
				t.Errorf("C121 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
		})
	}
}

func TestC107_OperandTypeMismatch(t *testing.T) {
	cases := []struct {
		expr string
		want int
	}{
		{"count > 0", 0},          // int vs int
		{"family == 'claude'", 0}, // string vs string
		{"approved == true", 0},   // bool vs bool
		{"count == 'x'", 1},       // int vs string
		{"count < 'x'", 1},        // int vs string
		{"tags == 0", 1},          // string[] vs int
		{"blob == 0", 0},          // json → no opinion
		{"length(tags) > 0", 0},   // length()→int vs int
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			r := compileFile(t, whenExprTypeSrc(tc.expr))
			if got := countCode(r, DiagExprOperandTypeMismatch); got != tc.want {
				t.Errorf("C107 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
		})
	}
}

func TestC108_WhenExprNotBoolish(t *testing.T) {
	cases := []struct {
		expr string
		want int
	}{
		{"approved", 0},  // bare bool — the normal form
		{"count", 1},     // bare int — almost certainly a missing comparison
		{"tags", 0},      // bare string[] — truthy "non-empty" idiom is fine
		{"blob", 0},      // bare json — no opinion
		{"count > 0", 0}, // comparison → bool
		{"name", 0},      // bare string — truthy idiom, not flagged
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			r := compileFile(t, whenExprTypeSrc(tc.expr))
			if got := countCode(r, DiagWhenExprNotBoolish); got != tc.want {
				t.Errorf("C108 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
		})
	}
}

// TestExprTypeSeverities pins C121 as an error and C107/C108 as warnings.
func TestExprTypeSeverities(t *testing.T) {
	check := func(expr string, code DiagCode, wantSev Severity) {
		r := compileFile(t, whenExprTypeSrc(expr))
		found := false
		for _, d := range r.Diagnostics {
			if d.Code == code {
				found = true
				if d.Severity != wantSev {
					t.Errorf("%s severity = %s, want %s", code, d.Severity, wantSev)
				}
			}
		}
		if !found {
			t.Fatalf("expected a %s diagnostic for %q, got %v", code, expr, r.Diagnostics)
		}
	}
	check("family == 'cluade'", DiagEnumLiteralMismatch, SeverityError)
	check("count == 'x'", DiagExprOperandTypeMismatch, SeverityWarning)
	check("count", DiagWhenExprNotBoolish, SeverityWarning)
}

// TestComputeExprEnumChecked confirms the same checks fire inside a compute
// node's expressions, resolving input.X against the compute node's input
// schema rather than an edge source's output.
func TestComputeExprEnumChecked(t *testing.T) {
	src := `
schema in_s:
  family: string [enum: "claude", "gpt"]

schema out_s:
  hit: bool

prompt sys:
  hi

agent producer:
  backend: "claw"
  model: "anthropic/claude-sonnet-4-6"
  output: in_s
  system: sys

compute c:
  input: in_s
  output: out_s
  expr:
    hit: "family == 'cluade'"

workflow w:
  entry: producer
  producer -> c with { family: "{{outputs.producer.family}}" }
  c -> done
`
	r := compileFile(t, src)
	if got := countCode(r, DiagEnumLiteralMismatch); got != 1 {
		t.Errorf("expected 1 C121 from the compute expression, got %d\ndiagnostics: %v", got, r.Diagnostics)
	}
}

// TestC113_IndexOnScalar verifies subscripting a statically-scalar value warns,
// while subscripting an array (string[]) or json does not.
func TestC113_IndexOnScalar(t *testing.T) {
	cases := []struct {
		expr string
		want int
	}{
		{`count[0] == 1`, 1},  // int is scalar → not indexable
		{`name[0] == 'a'`, 1}, // string is scalar → not indexable
		{`tags[0] == 'a'`, 0}, // string[] is indexable
		{`blob[0] == 1`, 0},   // json → unknown, bail
	}
	for _, tc := range cases {
		r := compileFile(t, whenExprTypeSrc(tc.expr))
		if got := countCode(r, DiagIndexOnScalar); got != tc.want {
			t.Errorf("C113 for %q = %d, want %d (%v)", tc.expr, got, tc.want, r.Diagnostics)
		}
	}
}

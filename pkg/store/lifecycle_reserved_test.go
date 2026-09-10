package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// ReservedFailureCodes is hand-written, and the compiler (C248) refuses a
// bot's `code:` against it — so a constant added to the block above and
// forgotten here would be a code the engine reads as control flow while
// letting any workflow mint it. Parse the declarations rather than trust
// the list: a hand-copied vocabulary drifts, and this is the drift.
func TestReservedFailureCodesCoversEveryDeclaredConstant(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "lifecycle.go", nil, 0)
	if err != nil {
		t.Fatalf("parse lifecycle.go: %v", err)
	}

	declared := map[string]FailureCode{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		// `Name FailureCode = "VALUE"` — the const block's shape. A
		// grouped const carries the type on the first spec only, so
		// specs with no type but a string literal value count too.
		ident, isIdent := spec.Type.(*ast.Ident)
		if spec.Type != nil && (!isIdent || ident.Name != "FailureCode") {
			return true
		}
		for i, name := range spec.Names {
			if i >= len(spec.Values) {
				continue
			}
			lit, ok := spec.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			declared[name.Name] = FailureCode(lit.Value[1 : len(lit.Value)-1])
		}
		return true
	})

	if len(declared) == 0 {
		t.Fatal("parsed no FailureCode constants — the guard would pass vacuously")
	}

	for name, code := range declared {
		if !code.Reserved() {
			t.Errorf("%s (%q) is an engine failure code but is missing from ReservedFailureCodes — "+
				"a workflow could mint it as its own `fail` code and be treated as engine control flow", name, code)
		}
	}
	if len(ReservedFailureCodes) != len(declared) {
		t.Errorf("ReservedFailureCodes has %d entries, %d constants are declared — the list carries a value the block does not",
			len(ReservedFailureCodes), len(declared))
	}
}

// The empty code means UNKNOWN, never "no failure", and a bot's own
// vocabulary must pass.
func TestReservedFailureCodeBoundaries(t *testing.T) {
	if FailureCode("").Reserved() {
		t.Error("the empty code is reserved; it means UNKNOWN and no workflow declares it")
	}
	for _, own := range []FailureCode{"PLAN_BUDGET_EXHAUSTED", "LOT_NOT_ACTIONABLE", "NOTHING_TO_DO"} {
		if own.Reserved() {
			t.Errorf("%q is refused as reserved but is a bot's own vocabulary", own)
		}
	}
}

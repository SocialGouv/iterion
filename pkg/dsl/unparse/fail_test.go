package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// The studio round-trips a workflow through unparse on every save. A field
// it does not write is a field an operator loses by opening the editor —
// so a typed refusal must survive parse → unparse → re-parse intact.
func TestUnparseRoundtripTypedFail(t *testing.T) {
	src := `compute gate:
  expr:
    pct: "42"

fail plan_exhausted:
  description: "the plan phase outgrew its share"
  code: PLAN_BUDGET_EXHAUSTED
  message: "planning used {{outputs.gate.pct}}% of the budget"
  resumable: true

fail not_actionable:
  code: LOT_NOT_ACTIONABLE

workflow w:
  entry: gate
  gate -> plan_exhausted when too_slow
  gate -> not_actionable else
`
	first := parser.Parse("t.bot", src)
	for _, d := range first.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s", d.Error())
		}
	}

	out := unparse.Unparse(first.File)
	if !strings.Contains(out, "fail plan_exhausted:") {
		t.Fatalf("unparse dropped the fail declaration:\n%s", out)
	}

	second := parser.Parse("t.bot", out)
	for _, d := range second.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("re-parse error: %s\n--- unparsed ---\n%s", d.Error(), out)
		}
	}
	if len(second.File.Fails) != 2 {
		t.Fatalf("re-parsed %d fail decls, want 2:\n%s", len(second.File.Fails), out)
	}
	got := second.File.Fails[0]
	want := first.File.Fails[0]
	if got.Name != want.Name || got.Description != want.Description ||
		got.Code != want.Code || got.Message != want.Message || got.Resumable != want.Resumable {
		t.Errorf("round trip = %+v, want %+v\n%s", *got, *want, out)
	}
	if second.File.Fails[1].Resumable {
		t.Error("a fail node with no `resumable:` came back resumable")
	}
}

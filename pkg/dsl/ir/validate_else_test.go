package ir

import "testing"

// ---------------------------------------------------------------------------
// C015 / C039 / C040 — the `else` fallback edge contract
// ---------------------------------------------------------------------------

// elseSrc builds a judge with the given edge block over a bool schema.
func elseSrc(edges string) string {
	return `
schema s:
  approved: bool

prompt sys:
  System.

prompt usr:
  User.

judge check:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent alt:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: check
` + edges
}

func TestValidateElse_SatisfiesMissingFallback(t *testing.T) {
	r := compileFile(t, elseSrc(`  check -> done when approved
  check -> alt else
  alt -> done
`))
	expectNoDiag(t, r, DiagMissingFallback)
	expectNoDiag(t, r, DiagMultipleDefaultEdges)
	expectNoDiag(t, r, DiagElseWithoutConditional)
	expectNoDiag(t, r, DiagMultipleElseEdges)
	expectNoDiag(t, r, DiagElseWithUnconditional)
}

func TestValidateElse_WithoutConditionalSibling_Rejected(t *testing.T) {
	r := compileFile(t, elseSrc(`  check -> alt else
  alt -> done
`))
	expectDiag(t, r, DiagElseWithoutConditional)
}

func TestValidateElse_Multiple_Rejected(t *testing.T) {
	r := compileFile(t, elseSrc(`  check -> done when approved
  check -> alt else
  check -> fail else
  alt -> done
`))
	expectDiag(t, r, DiagMultipleElseEdges)
}

func TestValidateElse_AlongsideUnconditional_Rejected(t *testing.T) {
	r := compileFile(t, elseSrc(`  check -> done when approved
  check -> alt else
  check -> fail
  alt -> done
`))
	expectDiag(t, r, DiagElseWithUnconditional)
}

func TestCompileElse_CarriesFlagToIR(t *testing.T) {
	wf := mustCompile(t, elseSrc(`  check -> done when approved
  check -> alt else
  alt -> done
`))
	found := false
	for _, e := range wf.Edges {
		if e.From == "check" && e.To == "alt" {
			found = true
			if !e.IsElse {
				t.Fatal("check->alt lost IsElse in IR")
			}
			if e.IsConditional() {
				t.Fatal("else edge must not be conditional")
			}
		}
	}
	if !found {
		t.Fatal("check->alt edge missing from IR")
	}
}

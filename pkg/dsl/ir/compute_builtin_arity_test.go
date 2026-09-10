package ir

import (
	"strings"
	"testing"
)

// A builtin call the evaluator cannot satisfy must be refused at COMPILE,
// on every launch surface, instead of compiling clean and dying mid-run.
//
// Production shape (#858): a bot authored against a newer engine called
// `max(a, b, c)`; the runner's evaluator had a binary `max`. Function calls
// parse generically, so the launch compiled, the run started, and
// `compute "delivery_reserve"` died at `expr: max() takes …` — a
// deterministic failure discovered after a sandbox, a clone and a plan
// phase had been paid for.
//
// The arity is declared once, beside the builtin registry the evaluator
// dispatches from, so the compiler and the evaluator cannot disagree.
func TestComputeBuiltinArityIsRefusedAtCompile(t *testing.T) {
	cases := []struct {
		name    string
		call    string
		wantMsg string
	}{
		// The production shape, expressed against a builtin that IS fixed-arity
		// on this engine: too many arguments.
		{"too many arguments", "length(input.n, input.n)", "length() takes 1 argument"},
		// The mirror: too few.
		{"too few arguments", "slice(input.n, 1)", "slice() takes 3 arguments"},
		// A variadic builtin still has a floor.
		{"variadic floor", "max()", "max() takes at least 1 argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := compileFile(t, arityWorkflow(tc.call))
			var found *Diagnostic
			for i, d := range res.Diagnostics {
				if d.Code == DiagBuiltinArity {
					found = &res.Diagnostics[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("no %s diagnostic for %q — the call compiles and only fails at run time:\n%s",
					DiagBuiltinArity, tc.call, renderDiags(res.Diagnostics))
			}
			if found.Severity != SeverityError {
				t.Errorf("%s severity = %v, want error — a compute that cannot evaluate must not launch", DiagBuiltinArity, found.Severity)
			}
			if !strings.Contains(found.Message, tc.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", found.Message, tc.wantMsg)
			}
			if !strings.Contains(found.Message, "derive") {
				t.Errorf("message = %q, want it to name the compute node and field", found.Message)
			}
		})
	}
}

// The same guard on an edge's `when "expr"` clause: a condition that cannot
// evaluate decides no edge, and the run dies selecting one.
func TestEdgeWhenBuiltinArityIsRefusedAtCompile(t *testing.T) {
	src := `
schema n:
  n: int

schema out:
  ok: bool

agent start:
  model: "claude-opus-4-7"
  output: n

compute derive:
  output: out
  expr:
    ok: "input.n > 0"

workflow w:
  entry: start
  start -> derive when "contains(input.n)"
  start -> done
  derive -> done
`
	res := compileFile(t, src)
	for _, d := range res.Diagnostics {
		if d.Code == DiagBuiltinArity && d.Severity == SeverityError {
			return
		}
	}
	t.Fatalf("no %s error for an edge `when` with a bad arity:\n%s", DiagBuiltinArity, renderDiags(res.Diagnostics))
}

// An unknown builtin is already refused at parse (C040). Pinned here so the
// two halves of "an old engine refuses a new bot up front" stay together:
// a NAME the evaluator does not have, and an ARITY it cannot satisfy.
func TestComputeUnknownBuiltinIsRefusedAtCompile(t *testing.T) {
	res := compileFile(t, arityWorkflow("clamp(input.n, 0, 1)"))
	for _, d := range res.Diagnostics {
		if d.Severity == SeverityError && strings.Contains(d.Message, `unknown function "clamp"`) {
			return
		}
	}
	t.Fatalf("an unknown builtin compiled clean:\n%s", renderDiags(res.Diagnostics))
}

func arityWorkflow(call string) string {
	return `
schema n:
  n: int

schema out:
  v: int

agent start:
  model: "claude-opus-4-7"
  output: n

compute derive:
  output: out
  expr:
    v: "` + call + `"

workflow w:
  entry: start
  start -> derive with {n: "{{outputs.start.n}}"}
  derive -> done
`
}

func renderDiags(diags []Diagnostic) string {
	var b strings.Builder
	for _, d := range diags {
		b.WriteString("  " + string(d.Code) + " " + d.Message + "\n")
	}
	if b.Len() == 0 {
		return "  (no diagnostics)"
	}
	return b.String()
}

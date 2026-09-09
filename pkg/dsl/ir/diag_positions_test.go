package ir

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A finding must point at the line the author has to EDIT: the edge line for
// an edge-scoped check (each edge its own line, even when several share
// endpoints), the prompt declaration for a reference inside a prompt body —
// never the header of the node that merely consumes the text.
func TestDiagnosticsPointAtTheEdgeAndPromptLines(t *testing.T) {
	lines := []string{
		/* 1 */ "schema out:",
		/* 2 */ "  ok: bool",
		/* 3 */ "",
		/* 4 */ "prompt shared:",
		/* 5 */ "  Uses {{outputs.ghost.field}} here.",
		/* 6 */ "",
		/* 7 */ "agent a:",
		/* 8 */ "  model: \"m\"",
		/* 9 */ "  output: out",
		/* 10 */ "  system: shared",
		/* 11 */ "",
		/* 12 */ "router r:",
		/* 13 */ "  mode: llm",
		/* 14 */ "  model: \"m\"",
		/* 15 */ "",
		/* 16 */ "agent b:",
		/* 17 */ "  model: \"m\"",
		/* 18 */ "  output: out",
		/* 19 */ "  system: shared",
		/* 20 */ "",
		/* 21 */ "agent c:",
		/* 22 */ "  model: \"m\"",
		/* 23 */ "  output: out",
		/* 24 */ "",
		/* 25 */ "workflow w:",
		/* 26 */ "  entry: a",
		/* 27 */ "  a -> r",
		/* 28 */ "  r -> b when ok",
		/* 29 */ "  r -> b when not ok",
		/* 30 */ "  r -> c",
		/* 31 */ "  b -> done with { taken: \"{{input.nosuchfield}}\" }",
		/* 32 */ "  c -> done",
	}
	src := strings.Join(lines, "\n") + "\n"
	pr := parser.Parse("pos.bot", src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("unexpected parse diagnostic: %s", d.Error())
	}
	res := Compile(pr.File)

	at := map[string][]int{}
	for _, d := range res.Diagnostics {
		at[string(d.Code)] = append(at[string(d.Code)], d.Line)
	}
	// C022 fires once per conditional edge of the llm router: lines 28 and 29,
	// not 28 twice.
	if got := fmt.Sprint(at["C022"]); got != "[28 29]" {
		t.Errorf("C022 lines = %s, want [28 29] (each edge its own line)", got)
	}
	// The unknown-node reference lives in the prompt body: line 4 is where
	// to edit, once per consuming node.
	for _, l := range at["C029"] {
		if l != 4 {
			t.Errorf("C029 at line %d, want 4 (the prompt declaration)", l)
		}
	}
	if len(at["C029"]) == 0 {
		t.Errorf("expected C029 for the ghost reference, got %v", at)
	}
	// The with-mapping reference lives on the edge line.
	if got := fmt.Sprint(at["C034"]); got != "[31]" {
		t.Errorf("C034 lines = %s, want [31] (the edge line)", got)
	}

	// Determinism: the same file compiles to the same ordered list.
	first := diagSignature(res.Diagnostics)
	for i := 0; i < 5; i++ {
		again := Compile(parser.Parse("pos.bot", src).File)
		if s := diagSignature(again.Diagnostics); s != first {
			t.Fatalf("diagnostic order changed between two compilations:\n%s\nvs\n%s", first, s)
		}
	}
}

func diagSignature(ds []Diagnostic) string {
	var b strings.Builder
	for _, d := range ds {
		fmt.Fprintf(&b, "%s@%d:%d %s\n", d.Code, d.Line, d.Column, d.NodeID)
	}
	return b.String()
}

// The catalogue fix for C032 ("add an output: schema") cannot be followed on
// a router, which has no output of its own; that site carries its own fix.
func TestRouterPassThroughWarningCarriesItsOwnFix(t *testing.T) {
	src := `schema out:
  ok: bool

agent a:
  model: "m"
  output: out

router r:
  mode: condition

agent b:
  model: "m"
  output: out

workflow w:
  entry: a
  a -> r
  r -> b with { taken: "{{input.zzz}}" }
  b -> done
`
	res := Compile(parser.Parse("r.bot", src).File)
	var seen bool
	for _, d := range res.Diagnostics {
		if d.Code != DiagRefNodeNoSchema {
			continue
		}
		seen = true
		if strings.Contains(d.Hint, "output:` schema") || !strings.Contains(d.Hint, "map the field onto it") {
			t.Errorf("router C032 hint = %q — must tell the author to map the field onto the router, not to add an output schema a router cannot have", d.Hint)
		}
	}
	if !seen {
		t.Fatalf("expected a C032 warning on the router pass-through, got %+v", res.Diagnostics)
	}
}

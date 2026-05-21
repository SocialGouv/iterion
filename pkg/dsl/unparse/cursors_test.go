package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const cursorsRoundtripSrc = `cursor ambition:
  description: "How aggressively to expand scope"
  values:
    cautious: "Stick strictly to the stated request."
    balanced: "Address the request; suggest one improvement."
    ambitious: "Proactively identify 2-3 improvements."

cursor depth:
  description: "Investigation thoroughness"
  bands:
    "0.0..0.33": "Skim the surface."
    "0.34..0.66": "Examine main code paths."
    "0.67..1.0": "Trace all call sites."

agent reviewer:
  model: "anthropic/claude-opus-4-7"
  system: review_prompt
  cursors:
    ambition: ambitious
    depth: 0.7

judge gate:
  model: "anthropic/claude-sonnet-4-6"
  system: review_prompt
  cursors:
    enabled: false
    ambition: cautious

prompt review_prompt:
  Review carefully.

workflow w:
  entry: reviewer
  reviewer -> gate
  gate -> done
`

func TestCursorsRoundtrip(t *testing.T) {
	pr1 := parser.Parse("t.iter", cursorsRoundtripSrc)
	if len(pr1.Diagnostics) > 0 {
		for _, d := range pr1.Diagnostics {
			t.Logf("first parse diag: %+v", d)
		}
		t.Fatalf("first parse produced diagnostics")
	}
	out1 := Unparse(pr1.File)

	pr2 := parser.Parse("t.iter", out1)
	if len(pr2.Diagnostics) > 0 {
		for _, d := range pr2.Diagnostics {
			t.Logf("second parse diag: %+v", d)
		}
		t.Fatalf("re-parse of unparsed source produced diagnostics:\n%s", out1)
	}
	out2 := Unparse(pr2.File)

	if out1 != out2 {
		t.Fatalf("round-trip drift:\n--- pass 1 ---\n%s\n--- pass 2 ---\n%s", out1, out2)
	}

	// Spot-check that the unparsed form retains the cursor structure.
	expectedSubstrings := []string{
		"cursor ambition:",
		`cautious: "Stick strictly`,
		"cursor depth:",
		`"0.0..0.33":`,
		"cursors:",
		"ambition: ambitious",
		"depth: 0.7",
		"enabled: false",
	}
	for _, want := range expectedSubstrings {
		if !strings.Contains(out1, want) {
			t.Errorf("unparsed source missing %q\n---\n%s", want, out1)
		}
	}
}

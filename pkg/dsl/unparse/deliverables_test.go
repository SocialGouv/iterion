package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// TestUnparseDeliverables_RoundTrip holds the writer to the reader: a path
// is written QUOTED, so a document that declares `out/annex.html` reads back
// as the same single entry instead of being re-read as something else.
func TestUnparseDeliverables_RoundTrip(t *testing.T) {
	src := `agent reporter:
  model: "gpt-4"
  deliverables: ["report.md", "out/annex.html"]

judge gate:
  model: "gpt-4"
  deliverables: ["verdict.json"]

tool export:
  command: "make report"
  deliverables: ["dist/report.zip"]

workflow w:
  entry: reporter

  reporter -> gate
  gate -> export
  export -> done
`
	res := parsertest.Parse("test.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", res.Diagnostics)
	}

	out := unparse.Unparse(res.File)
	for _, want := range []string{
		`deliverables: ["report.md", "out/annex.html"]`,
		`deliverables: ["verdict.json"]`,
		`deliverables: ["dist/report.zip"]`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("unparse output missing %q:\n%s", want, out)
		}
	}

	res2 := parsertest.Parse("test.bot", out)
	if len(res2.Diagnostics) != 0 {
		t.Fatalf("reparse diagnostics: %+v\nsource:\n%s", res2.Diagnostics, out)
	}
	if got := strings.Join(res2.File.Agents[0].Deliverables, ","); got != "report.md,out/annex.html" {
		t.Fatalf("agent Deliverables after round-trip = %q", got)
	}
	if got := strings.Join(res2.File.Judges[0].Deliverables, ","); got != "verdict.json" {
		t.Fatalf("judge Deliverables after round-trip = %q", got)
	}
	if got := strings.Join(res2.File.Tools[0].Deliverables, ","); got != "dist/report.zip" {
		t.Fatalf("tool Deliverables after round-trip = %q", got)
	}
}

// TestUnparseDeliverables_AbsentStaysAbsent: a node that declares nothing
// must not grow an empty list on the way out — an unparse that writes
// `deliverables: []` would turn "undeclared" into "declared empty".
func TestUnparseDeliverables_AbsentStaysAbsent(t *testing.T) {
	src := `agent reporter:
  model: "gpt-4"

workflow w:
  entry: reporter

  reporter -> done
`
	res := parsertest.Parse("test.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", res.Diagnostics)
	}
	if out := unparse.Unparse(res.File); strings.Contains(out, "deliverables") {
		t.Fatalf("unparse invented a deliverables line:\n%s", out)
	}
}

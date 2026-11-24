package spec_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// spanTables reads, for each kind whose declaration keeps a per-property
// span table (ast.PropertySpans), the table of the declaration the kind's
// probe document parses to.
var spanTables = map[string]func(*ast.File) ast.PropertySpans{
	"workflow":      func(f *ast.File) ast.PropertySpans { return f.Workflows[0].Properties },
	"agent":         func(f *ast.File) ast.PropertySpans { return f.Agents[0].Properties },
	"judge":         func(f *ast.File) ast.PropertySpans { return f.Judges[0].Properties },
	"router":        func(f *ast.File) ast.PropertySpans { return f.Routers[0].Properties },
	"human":         func(f *ast.File) ast.PropertySpans { return f.Humans[0].Properties },
	"tool":          func(f *ast.File) ast.PropertySpans { return f.Tools[0].Properties },
	"compute":       func(f *ast.File) ast.PropertySpans { return f.Computes[0].Properties },
	"subbot":        func(f *ast.File) ast.PropertySpans { return f.Subbots[0].Properties },
	"emit":          func(f *ast.File) ast.PropertySpans { return f.Emits[0].Properties },
	"wait":          func(f *ast.File) ast.PropertySpans { return f.Waits[0].Properties },
	"await_answers": func(f *ast.File) ast.PropertySpans { return f.AwaitAnswers[0].Properties },
	"fail":          func(f *ast.File) ast.PropertySpans { return f.Fails[0].Properties },
}

// TestEveryPropertyOfEveryNodeKindKeepsItsSpan: every node kind of the
// registry, and the workflow, keeps a per-property span table, and every
// property the registry lists for it — written with each well-formed value
// of its form — is recorded there at the line and column its name starts
// and spanning past the name. A node kind added to the registry without a
// table, or a property a declaration loop reads without recording, is red
// here (#1094).
func TestEveryPropertyOfEveryNodeKindKeepsItsSpan(t *testing.T) {
	n := 0
	for _, k := range spec.Kinds {
		if k.Role != spec.Node && k.Name != "workflow" {
			continue
		}
		table, ok := spanTables[k.Name]
		if !ok {
			t.Errorf("kind %q keeps no per-property span table (spanTables)", k.Name)
			continue
		}
		tmpl := probes[k.Name]
		line := strings.Replace(tmpl, "%s: 1", "%s", 1)
		at := strings.Index(line, "%s")
		indent := line[strings.LastIndex(line[:at], "\n")+1 : at]
		wantLine := strings.Count(line[:at], "\n") + 1
		for _, p := range k.Properties {
			for _, sample := range sampleValues(p) {
				n++
				doc := fmt.Sprintf(line, strings.ReplaceAll(sample, "\n", "\n"+indent))
				res := parsertest.Parse("probe.bot", doc)
				if len(res.Diagnostics) > 0 {
					continue // TestEveryListedPropertyParsesCleanWithItsForm judges that
				}
				sp, ok := table(res.File).Lookup(p.Name)
				if !ok {
					t.Errorf("%s.%s: parsed from %q, no span recorded", k.Name, p.Name, doc)
					continue
				}
				if sp.Start.Line != wantLine || sp.Start.Column != len(indent)+1 {
					t.Errorf("%s.%s: span starts at %d:%d, the name is written at %d:%d", k.Name, p.Name, sp.Start.Line, sp.Start.Column, wantLine, len(indent)+1)
				}
				if sp.End.Line < sp.Start.Line || (sp.End.Line == sp.Start.Line && sp.End.Column <= sp.Start.Column+len(p.Name)) {
					t.Errorf("%s.%s: span ends at %d:%d, before the end of %q", k.Name, p.Name, sp.End.Line, sp.End.Column, sample)
				}
			}
		}
	}
	if n < 300 {
		t.Fatalf("only %d documents probed — the sweep is not covering the registry", n)
	}
}

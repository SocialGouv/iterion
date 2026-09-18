package bots

import (
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// TestExampleCursorCatalogueCompiles: examples/cursors/cursors.bot is a
// copy-paste catalogue of `cursor` blocks with no workflow of its own, so
// no other gate ever compiles its cursors (`iterion validate` stops at "no
// workflow found"). This test pastes the whole file into a minimal workflow
// that activates every cursor and requires a clean compile — overlapping
// bands (C085) or a band outside [0,1] would otherwise ship in the file the
// docs call copy-paste-ready.
func TestExampleCursorCatalogueCompiles(t *testing.T) {
	src, err := os.ReadFile("../examples/cursors/cursors.bot")
	if err != nil {
		t.Fatal(err)
	}
	catalogue := string(src)
	// Every `cursor <name>:` of the catalogue is activated on the node, a
	// numeric band for a `bands:` cursor, the first value for a `values:` one.
	var activations []string
	for _, line := range strings.Split(catalogue, "\n") {
		if !strings.HasPrefix(line, "cursor ") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(line, "cursor "), ":")
		block := catalogue[strings.Index(catalogue, line):]
		if strings.Contains(strings.SplitN(block, "\ncursor ", 2)[0], "\n  bands:") {
			activations = append(activations, "    "+name+": 0.5")
		} else {
			activations = append(activations, "    "+name+": "+firstValue(t, block))
		}
	}
	if len(activations) < 2 {
		t.Fatalf("the catalogue declares %d cursor(s); the test expects several", len(activations))
	}
	program := catalogue + "\nschema verdict:\n  rationale: string\n\nprompt sys:\n  Review.\n\nagent reviewer:\n  model: \"anthropic/claude-sonnet-4-6\"\n  system: sys\n  output: verdict\n  cursors:\n    enabled: true\n" +
		strings.Join(activations, "\n") + "\n\nworkflow main:\n  entry: reviewer\n  reviewer -> done\n"
	pr := parser.Parse("cursors-catalogue.bot", program)
	if pr.File == nil {
		t.Fatalf("the pasted catalogue does not parse: %v", pr.Diagnostics)
	}
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Errorf("parse: %s", d.Error())
		}
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatal("the pasted catalogue compiles to no workflow")
	}
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Errorf("compile: %s", d.Error())
		}
	}
}

// firstValue returns the first key of a `values:` cursor block.
func firstValue(t *testing.T, block string) string {
	t.Helper()
	after := strings.SplitN(block, "\n  values:\n", 2)
	if len(after) != 2 {
		t.Fatalf("no values: block in\n%s", block)
	}
	first := strings.SplitN(after[1], "\n", 2)[0]
	key := strings.TrimSpace(strings.SplitN(first, ":", 2)[0])
	if key == "" {
		t.Fatalf("no first value in\n%s", block)
	}
	return key
}

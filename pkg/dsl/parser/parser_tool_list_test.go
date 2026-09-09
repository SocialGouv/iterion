package parser

import (
	"reflect"
	"testing"
)

// A quoted element of a tool-reference list is the literal name: the form
// a name that is not an identifier has to take — a kebab-case artifact label
// from the studio canvas — and the form the unparser writes for it. It used
// to be dropped in silence, so a document carrying one could never be saved.
func TestToolListAcceptsAQuotedElement(t *testing.T) {
	src := "agent a:\n  model: \"m\"\n  artifact_labels: [\"review-ledger\", plan]\n  tools: [bash, \"my-tool\", mcp.srv.*]\n\nworkflow w:\n  entry: a\n  a -> done\n"
	pr := Parse("x.bot", src)
	for _, d := range pr.Diagnostics {
		t.Errorf("unexpected diagnostic: %s", d.Error())
	}
	a := pr.File.Agents[0]
	if want := []string{"review-ledger", "plan"}; !reflect.DeepEqual(a.ArtifactLabels, want) {
		t.Fatalf("artifact_labels = %v, want %v", a.ArtifactLabels, want)
	}
	if want := []string{"bash", "my-tool", "mcp.srv.*"}; !reflect.DeepEqual(a.Tools, want) {
		t.Fatalf("tools = %v, want %v", a.Tools, want)
	}
}

package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A `use` of a group that declares no node expands to nothing — a
// declaration the studio has not filled in yet, or a body that landed at
// the wrong indentation after a blank line. Nothing else reports it unless
// something references a node of the instance, so the compiler says so.
func TestUseOfAnEmptyGroupIsWarned(t *testing.T) {
	src := "group g:\n\nuse g as x\n\ntool t:\n  command: \"true\"\n\nworkflow w:\n  entry: t\n  t -> done\n"
	pr := parser.Parse("x.bot", src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("parse: %s", d.Error())
	}
	cr := Compile(pr.File)
	if cr.HasErrors() {
		t.Fatalf("compile errors: %v", cr.Diagnostics)
	}
	for _, d := range cr.Diagnostics {
		if d.Code == DiagCode("C141") {
			if d.Severity != SeverityWarning || !strings.Contains(d.Message, "empty") {
				t.Fatalf("C141 should be a warning naming the empty group: %v", d)
			}
			return
		}
	}
	t.Fatalf("no warning for a use of an empty group: %v", cr.Diagnostics)
}

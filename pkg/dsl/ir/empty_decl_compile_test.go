package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A bare `supervisor NAME:` — what the studio saves the moment a
// supervisor is created — must not spawn a live run-scoped supervisor
// with the default budget: it is warned about and not armed.
func TestEmptySupervisorIsNotArmed(t *testing.T) {
	src := "supervisor persy:\n\ntool t:\n  command: \"true\"\n\nworkflow w:\n  entry: t\n  t -> done\n"
	pr := parser.Parse("x.bot", src)
	if len(pr.Diagnostics) != 0 {
		t.Fatalf("parse: %v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	if cr.HasErrors() {
		t.Fatalf("compile errors: %v", cr.Diagnostics)
	}
	if len(cr.Workflow.Supervisors) != 0 {
		t.Fatalf("an empty supervisor was armed: %+v", cr.Workflow.Supervisors[0])
	}
	var warned bool
	for _, d := range cr.Diagnostics {
		if d.Code == DiagMalformedSupervisor && strings.Contains(d.Message, "is empty") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warning names the empty supervisor: %v", cr.Diagnostics)
	}
}

// A bare `workflow w:` compiles to the diagnostic the author can act on —
// the workflow declares no entry — not to a lookup of a node named "".
func TestEmptyWorkflowNamesWhatItLacks(t *testing.T) {
	pr := parser.Parse("x.bot", "workflow w:\n")
	if len(pr.Diagnostics) != 0 {
		t.Fatalf("parse: %v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Code == DiagMissingEntry {
			if !strings.Contains(d.Message, "declares no entry") {
				t.Fatalf("C008 on an empty workflow reads %q; want it to say the workflow declares no entry", d.Message)
			}
			return
		}
	}
	t.Fatalf("no C008 for a workflow with no entry: %v", cr.Diagnostics)
}

// A node whose output schema has no field is warned about (C140): the
// schema is a declaration the studio has not filled in, not a contract a
// model can fill.
func TestNodeReferencingAnEmptySchemaIsWarned(t *testing.T) {
	src := "schema verdict:\n\nprompt p:\n  judge it\n\nagent a:\n  model: \"m\"\n  output: verdict\n  system: p\n\nworkflow w:\n  entry: a\n  a -> done\n"
	pr := parser.Parse("x.bot", src)
	if len(pr.Diagnostics) != 0 {
		t.Fatalf("parse: %v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	var warned bool
	for _, d := range cr.Diagnostics {
		if d.Code == DiagEmptySchema {
			warned = true
			if d.Severity != SeverityWarning {
				t.Fatalf("C140 should be a warning, got %v", d.Severity)
			}
		}
	}
	if !warned {
		t.Fatalf("no C140 for a node whose output schema has no field: %v", cr.Diagnostics)
	}
}

package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func TestUnparseMemory_OnAgent(t *testing.T) {
	enabled := true
	scope := "session-continuity"
	read := true
	write := true
	inject := true
	f := &ast.File{
		Agents: []*ast.AgentDecl{{
			Name:    "reviser",
			Backend: "claw",
			Memory: &ast.MemoryBlock{
				Enabled:          &enabled,
				Scope:            &scope,
				Autoload:         []string{"INDEX.md", "CONTEXT_BRIEF.md"},
				Read:             &read,
				Write:            &write,
				PreCompactInject: &inject,
			},
		}},
		Workflows: []*ast.WorkflowDecl{{
			Name:  "flow",
			Entry: "reviser",
			Edges: []*ast.Edge{{From: "reviser", To: "done"}},
		}},
	}

	got := Unparse(f)
	checks := []string{
		"agent reviser:",
		"  memory:",
		"    enabled: true",
		"    scope: \"session-continuity\"",
		"    autoload: [\"INDEX.md\", \"CONTEXT_BRIEF.md\"]",
		"    read: true",
		"    write: true",
		"    pre_compact_inject: true",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q\nfull:\n%s", want, got)
		}
	}
}

func TestMemory_ParseUnparseRoundtrip(t *testing.T) {
	src := `agent reviser:
  backend: "claw"
  model: "openai/gpt-5.5"
  memory:
    enabled: true
    scope: "session-continuity"
    autoload: ["INDEX.md"]
    read: true
    write: true
    pre_compact_inject: true

workflow flow:
  entry: reviser

  reviser -> done
`
	pr1 := parser.Parse("test.bot", src)
	if hasErrors(pr1.Diagnostics) {
		t.Fatalf("first parse: %v", pr1.Diagnostics)
	}
	first := pr1.File
	mb := first.Agents[0].Memory
	if mb == nil || mb.Enabled == nil || !*mb.Enabled {
		t.Fatalf("first parse: Memory missing or disabled: %+v", mb)
	}
	if mb.Scope == nil || *mb.Scope != "session-continuity" {
		t.Fatalf("first parse: scope: %+v", mb.Scope)
	}
	if len(mb.Autoload) != 1 || mb.Autoload[0] != "INDEX.md" {
		t.Fatalf("first parse: autoload: %+v", mb.Autoload)
	}

	round := Unparse(first)
	pr2 := parser.Parse("test.bot", round)
	if hasErrors(pr2.Diagnostics) {
		t.Fatalf("second parse: %v\n%s", pr2.Diagnostics, round)
	}
	mb2 := pr2.File.Agents[0].Memory
	if mb2 == nil ||
		*mb2.Enabled != *mb.Enabled ||
		*mb2.Scope != *mb.Scope ||
		len(mb2.Autoload) != len(mb.Autoload) ||
		*mb2.Read != *mb.Read ||
		*mb2.Write != *mb.Write ||
		*mb2.PreCompactInject != *mb.PreCompactInject {
		t.Fatalf("roundtrip mismatch:\nbefore=%+v\nafter=%+v\nunparsed:\n%s", mb, mb2, round)
	}
}

// TestMemory_ProjectRootRoundtrip locks the new `project_root: true`
// flag's full pipeline: parse → AST → unparse → re-parse → AST. Without
// this, a future rename in the parser or unparser would silently drop
// the flag and break the shared-findings handoff between dispatcher
// bots and Nexie.
func TestMemory_ProjectRootRoundtrip(t *testing.T) {
	src := `agent finder:
  backend: "claw"
  model: "openai/gpt-5.5"
  memory:
    enabled: true
    scope: "findings"
    write: true
    project_root: true

workflow flow:
  entry: finder

  finder -> done
`
	pr := parser.Parse("test.bot", src)
	if hasErrors(pr.Diagnostics) {
		t.Fatalf("parse: %v", pr.Diagnostics)
	}
	mb := pr.File.Agents[0].Memory
	if mb == nil || mb.ProjectRoot == nil || !*mb.ProjectRoot {
		t.Fatalf("project_root not parsed: %+v", mb)
	}
	round := Unparse(pr.File)
	if !strings.Contains(round, "project_root: true") {
		t.Fatalf("unparse dropped project_root:\n%s", round)
	}
	pr2 := parser.Parse("test.bot", round)
	if hasErrors(pr2.Diagnostics) {
		t.Fatalf("second parse: %v\n%s", pr2.Diagnostics, round)
	}
	mb2 := pr2.File.Agents[0].Memory
	if mb2.ProjectRoot == nil || *mb2.ProjectRoot != *mb.ProjectRoot {
		t.Fatalf("roundtrip lost project_root: before=%+v after=%+v",
			mb.ProjectRoot, mb2.ProjectRoot)
	}
}

func TestMemory_UnknownPropertyDiagnostic(t *testing.T) {
	src := `agent reviser:
  backend: "claw"
  memory:
    enabled: true
    scope: "x"
    bogus_field: true

workflow flow:
  entry: reviser

  reviser -> done
`
	pr := parser.Parse("test.bot", src)
	found := false
	for _, d := range pr.Diagnostics {
		if strings.Contains(d.Message, "bogus_field") || strings.Contains(d.Message, "memory property") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected unknown-property diagnostic, got %+v", pr.Diagnostics)
	}
}

func hasErrors(diags []parser.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			return true
		}
	}
	return false
}

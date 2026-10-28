package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
)

// TestValidateMemory_GlobalWriteIsWarned pins C319: a node declaring
// `visibility: "global"` keeps `write:` true by DEFAULT, which is exactly how
// the ungated write door was opened — the author wrote one line and got a
// run-writable instance-wide space. The engine now withholds memory_write
// there (a run holds no platform authority), so the author must be told at
// compile time instead of discovering a missing tool at turn three.
//
// A WARNING, not an error: the combination stays expressible and installs the
// tool the day a super-admin run's authority reaches the engine. An error
// would remove a capability to close a hole, which D0 refused.
func TestValidateMemory_GlobalWriteIsWarned(t *testing.T) {
	mk := func(vis string, write bool) *compiler {
		w := &Workflow{Name: "t", Nodes: map[string]Node{
			"a1": &AgentNode{
				BaseNode:  BaseNode{ID: "a1"},
				LLMFields: LLMFields{Backend: "claw"},
				Memory:    &Memory{Enabled: true, Scope: "shared", Visibility: vis, Read: true, Write: write},
			},
		}}
		c := &compiler{}
		c.validateMemory(w)
		return c
	}
	find := func(c *compiler, code DiagCode) (Diagnostic, bool) {
		for _, d := range c.diags {
			if d.Code == code {
				return d, true
			}
		}
		return Diagnostic{}, false
	}

	d, ok := find(mk("global", true), DiagMemoryGlobalWriteGated)
	if !ok {
		t.Fatalf("no C319 for visibility: global with write: true — the author gets no warning that memory_write is withheld")
	}
	if d.Severity != SeverityWarning {
		t.Errorf("C319 severity = %v, want a warning (an error would remove the capability)", d.Severity)
	}
	if !strings.Contains(d.Message, "memory_write") || !strings.Contains(d.Message, "global") {
		t.Errorf("C319 must name the withheld tool and the visibility: %q", d.Message)
	}

	if _, ok := find(mk("global", false), DiagMemoryGlobalWriteGated); ok {
		t.Error("write: false asks for nothing the engine withholds — C319 must stay silent")
	}
	for _, vis := range []string{"org", "cross_project", "project", "bot", "user"} {
		if _, ok := find(mk(vis, true), DiagMemoryGlobalWriteGated); ok {
			t.Errorf("visibility %q is team-scoped and ungated — C319 must stay silent", vis)
		}
	}
}

// TestGlobalMemoryBotCompilesWithTheWarning drives the real author surface:
// the exact `.bot` block the ticket quotes, through the parser, so the
// diagnostic is proven to fire on source rather than on a hand-built IR (a
// field the parser never reaches would pass the unit test above).
func TestGlobalMemoryBotCompilesWithTheWarning(t *testing.T) {
	src := `
agent x:
  model: "anthropic/c"
  system: p
  backend: "claw"
  memory:
    enabled: true
    visibility: "global"
    scope: "shared"
    write: true

prompt p:
  hi

workflow w:
  entry: x
  x -> done
`
	pr := parsertest.Parse("t.bot", src)
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parse: %+v", pr.Diagnostics)
	}
	cr := Compile(pr.File)
	if cr.HasErrors() {
		t.Fatalf("the workflow must still COMPILE — the capability is gated, not refused: %+v", cr.Diagnostics)
	}
	var found bool
	for _, d := range cr.Diagnostics {
		if d.Code == DiagMemoryGlobalWriteGated {
			found = true
			if d.NodeID != "x" {
				t.Errorf("C319 node = %q, want the declaring node", d.NodeID)
			}
		}
	}
	if !found {
		t.Fatalf("no C319 on the ticket's own .bot shape: %+v", cr.Diagnostics)
	}
}

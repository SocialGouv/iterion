package ir

import (
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"testing"
)

func TestMemoryVisibilityCompiles(t *testing.T) {
	src := `
agent x:
  model: "anthropic/c"
  system: p
  backend: "claw"
  memory:
    enabled: true
    visibility: "org"
    scope: "conventions"

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
		t.Fatalf("compile: %+v", cr.Diagnostics)
	}
	n := cr.Workflow.Nodes["x"].(*AgentNode)
	if n.Memory == nil || n.Memory.Visibility != "org" || n.Memory.Scope != "conventions" {
		t.Fatalf("memory not compiled: %+v", n.Memory)
	}
}

// TestMemoryVisibilityBotCompiles pins the exact DSL shape Evoly uses —
// the first shipped consumer of per-bot memory (visibility: "bot"). It
// proves the full block (autoload + read + write + pre_compact_inject)
// survives compilation with the bot axis intact.
func TestMemoryVisibilityBotCompiles(t *testing.T) {
	src := `
agent x:
  model: "anthropic/c"
  system: p
  backend: "claw"
  memory:
    enabled: true
    visibility: "bot"
    scope: "vision"
    autoload: ["VISION.md", "CONTEXT_BRIEF.md"]
    read: true
    write: true
    pre_compact_inject: true

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
		t.Fatalf("compile: %+v", cr.Diagnostics)
	}
	n := cr.Workflow.Nodes["x"].(*AgentNode)
	if n.Memory == nil || n.Memory.Visibility != "bot" || n.Memory.Scope != "vision" {
		t.Fatalf("memory not compiled: %+v", n.Memory)
	}
	if !n.Memory.Read || !n.Memory.Write || !n.Memory.PreCompactInject {
		t.Fatalf("memory flags lost: %+v", n.Memory)
	}
	if len(n.Memory.Autoload) != 2 {
		t.Fatalf("autoload lost: %+v", n.Memory.Autoload)
	}
}

func TestValidateMemory_Visibility(t *testing.T) {
	mk := func(vis string, projectRoot bool) *compiler {
		w := &Workflow{Name: "t", Nodes: map[string]Node{
			"a1": &AgentNode{
				BaseNode:  BaseNode{ID: "a1"},
				LLMFields: LLMFields{Backend: "claw"},
				Memory:    &Memory{Enabled: true, Scope: "x", Visibility: vis, ProjectRoot: projectRoot, Read: true, Write: true},
			},
		}}
		c := &compiler{}
		c.validateMemory(w)
		return c
	}
	has := func(c *compiler, code DiagCode) bool {
		for _, d := range c.diags {
			if d.Code == code {
				return true
			}
		}
		return false
	}
	if c := mk("org", false); has(c, DiagMemoryInvalidVisibility) || has(c, DiagMemoryVisibilityConflict) {
		t.Fatalf("valid org wrongly flagged: %+v", c.diags)
	}
	if c := mk("weird", false); !has(c, DiagMemoryInvalidVisibility) {
		t.Fatal("expected C170 for unknown visibility")
	}
	if c := mk("project", true); !has(c, DiagMemoryVisibilityConflict) {
		t.Fatal("expected C171 for visibility + project_root")
	}
}

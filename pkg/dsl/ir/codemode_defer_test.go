package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const deferBot = `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    defer: on
    tool_calls: 128

workflow w:
  entry: a
  a -> done
`

// The deferral reaches the node through the whole chain, normalized like the
// mode is, and a node that declares none inherits nothing ("" reads as off).
func TestTheDeferCarriesToTheNode(t *testing.T) {
	t.Parallel()
	r := compileFile(t, deferBot)
	if len(r.Diagnostics) > 0 {
		t.Fatalf("a defer-only declaration drew diagnostics: %+v", r.Diagnostics)
	}
	n := r.Workflow.Nodes["a"].(*AgentNode)
	if n.CodemodeDefer != "on" {
		t.Errorf("CodemodeDefer = %q, want \"on\"", n.CodemodeDefer)
	}
	if n.Codemode != "on" {
		t.Errorf("Codemode = %q, want \"on\": the block form implies it beside a defer", n.Codemode)
	}
	if n.CodemodeBounds == nil {
		t.Error("the block's bounds vanished beside a defer line")
	}
}

func TestTheWorkflowRungCarriesTheDeferToo(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"

workflow w:
  entry: a
  codemode:
    defer: on
  a -> done
`)
	if len(r.Diagnostics) > 0 {
		t.Fatalf("a workflow defer drew diagnostics: %+v", r.Diagnostics)
	}
	if r.Workflow.CodemodeDefer != "on" {
		t.Errorf("workflow CodemodeDefer = %q, want \"on\"", r.Workflow.CodemodeDefer)
	}
	n := r.Workflow.Nodes["a"].(*AgentNode)
	if n.CodemodeDefer != "" {
		t.Errorf("node CodemodeDefer = %q, want \"\": the node wrote nothing and inherits through the resolver, not through a copied field", n.CodemodeDefer)
	}
}

// The value is normalized at the carry, exactly like the mode's: the AST
// keeps what the author wrote for the unparser, the IR keeps the decided
// value the executor resolves from.
func TestTheDeferIsNormalizedAtTheCarry(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    defer: ON

workflow w:
  entry: a
  a -> done
`)
	if len(r.Diagnostics) > 0 {
		t.Fatalf("a padded, uppercased defer drew diagnostics: %+v", r.Diagnostics)
	}
	n := r.Workflow.Nodes["a"].(*AgentNode)
	if n.CodemodeDefer != "on" {
		t.Errorf("CodemodeDefer = %q, want the normalized \"on\"", n.CodemodeDefer)
	}
}

func TestResolveCodemodeDeferTakesTheNearestRung(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name          string
		run, node, wf string
		want          string
	}{
		{"nothing anywhere is off", "", "", "", ""},
		{"the workflow answers an inheriting node", "", "", "on", "on"},
		{"the node beats the workflow", "", "off", "on", "off"},
		{"the run beats everything", "on", "off", "on", "on"},
		{"the run can keep it off too", "off", "on", "on", "off"},
	} {
		if got := ResolveCodemodeDefer(c.run, c.node, c.wf); got != c.want {
			t.Errorf("%s: ResolveCodemodeDefer(%q, %q, %q) = %q, want %q", c.name, c.run, c.node, c.wf, got, c.want)
		}
	}
}

// C354: the value is held to the same two spellings the mode is, and the
// error names them.
func TestAnInvalidDeferValueIsAnError(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    defer: later

workflow w:
  entry: a
  a -> done
`)
	if got := countCode(r, DiagInvalidCodemodeDefer); got != 1 {
		t.Fatalf("C354 on defer: later: %d time(s), want 1: %+v", got, r.Diagnostics)
	}
	for _, d := range r.Diagnostics {
		if d.Code == DiagInvalidCodemodeDefer && !strings.Contains(d.Error(), "on, off") {
			t.Errorf("the C354 message does not name the accepted values: %s", d.Error())
		}
	}
}

// C355, the deferral's own C348: inert beside the node's own `mode: off`,
// a warning and not an error. Off at the WORKFLOW rung is a default for the
// nodes that enable codemode themselves, so it draws nothing — the same
// asymmetry C348 holds for the bounds.
func TestDeferBesideModeOffIsInertAndWarned(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    mode: off
    defer: on

workflow w:
  entry: a
  a -> done
`)
	if got := countCode(r, DiagCodemodeDeferWithModeOff); got != 1 {
		t.Fatalf("C355 on defer beside mode: off: %d time(s), want 1: %+v", got, r.Diagnostics)
	}
	n := r.Workflow.Nodes["a"].(*AgentNode)
	if n.CodemodeDefer != "on" || n.Codemode != "off" {
		t.Errorf("the declaration was rewritten (defer=%q mode=%q), want both as written", n.CodemodeDefer, n.Codemode)
	}

	t.Run("a defer off beside mode off draws nothing", func(t *testing.T) {
		r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    mode: off
    defer: off

workflow w:
  entry: a
  a -> done
`)
		if got := countCode(r, DiagCodemodeDeferWithModeOff); got != 0 {
			t.Fatalf("C355 fired %d time(s) on the default value: %+v", got, r.Diagnostics)
		}
	})

	t.Run("the workflow rung is a default, not dead text", func(t *testing.T) {
		r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode: on

workflow w:
  entry: a
  codemode:
    mode: off
    defer: on
  a -> done
`)
		if got := countCode(r, DiagCodemodeDeferWithModeOff); got != 0 {
			t.Fatalf("C355 fired %d time(s) at the workflow rung: %+v", got, r.Diagnostics)
		}
	})
}

// The deferral has no diagnostic of its own where the capability itself is
// refused: C339 is the finding that fires, and it is an error, so the run
// never starts for the deferral to matter. The two conditions are disjoint —
// C339 needs the mode ON, the defer warning needs it off — which this pins
// from both sides.
func TestDeferOnAnUnreachableBackendFollowsC339(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claude_code"
  codemode:
    defer: on

workflow w:
  entry: a
  a -> done
`)
	if got := countCode(r, DiagCodemodeBackendUnsupported); got != 1 {
		t.Fatalf("C339 on codemode beside defer on a CLI backend: %d time(s), want 1: %+v", got, r.Diagnostics)
	}
	if got := countCode(r, DiagCodemodeDeferWithModeOff); got != 0 {
		t.Errorf("C355 fired beside a C339 — the refused capability is C339's finding, not the deferral's: %+v", r.Diagnostics)
	}
	if got := countCode(r, DiagInvalidCodemodeDefer); got != 0 {
		t.Errorf("C354 fired on a valid value: %+v", r.Diagnostics)
	}
}

// The transport carries the defer: a document saved with one reads back the
// same program through the JSON AST entry point, which bypasses the parser.
func TestTheTransportCarriesTheDefer(t *testing.T) {
	t.Parallel()
	res, err := parser.Parse("t.bot", deferBot)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	raw, err := ast.MarshalFile(res.File)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	file, err := ast.UnmarshalFile(raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := file.Agents[0].Codemode
	if got == nil || got.Defer != "on" {
		t.Fatalf("the transport lost the defer: %+v", got)
	}
	if got.Mode != "on" {
		t.Errorf("transport mode = %q, want \"on\": the block form implies it through the transport too", got.Mode)
	}

	// And the writer keeps it: a defer-only block must not collapse to the
	// short form, which has no way to say it.
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    defer: on

workflow w:
  entry: a
  a -> done
`)
	if r.Workflow.Nodes["a"].(*AgentNode).CodemodeDefer != "on" {
		t.Fatal("the compile fixture broke — the round-trip below would prove nothing")
	}
}

// The design note docs/dsl.md states: a block holding only `defer: off` is
// still a block, and a block implies `mode: on`. Pinned so the
// documentation cannot drift from the parser's implication.
func TestADeferOffOnlyBlockStillImpliesOn(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    defer: off

workflow w:
  entry: a
  a -> done
`)
	if len(r.Diagnostics) > 0 {
		t.Fatalf("a defer-off-only block drew diagnostics: %+v", r.Diagnostics)
	}
	n := r.Workflow.Nodes["a"].(*AgentNode)
	if n.Codemode != "on" || n.CodemodeDefer != "off" {
		t.Errorf("mode=%q defer=%q, want the block's implied on and the declared off", n.Codemode, n.CodemodeDefer)
	}
}

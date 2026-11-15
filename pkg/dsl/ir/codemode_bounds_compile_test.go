package ir

import (
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// The bounds from `.bot` TEXT, through the whole chain. Every test here
// starts from source for the reason codemode_backend_test.go states: a
// hand-built Workflow passes over the parser, the AST encoder and the
// compiler's carry, which is where three of the four things that can go
// wrong live.

func TestTheCodemodeBlockCarriesItsBoundsToTheNode(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    tool_calls: 256
    wall: "2m"
    cpu: "45s"
    memory: 536870912
    script_bytes: 524288
    result_bytes: 2097152
    tool_result_bytes: 131072

workflow w:
  entry: a
  a -> done
`)
	if len(r.Diagnostics) > 0 {
		t.Fatalf("a block entirely under the ceiling drew diagnostics: %+v", r.Diagnostics)
	}
	n, ok := r.Workflow.Nodes["a"].(*AgentNode)
	if !ok {
		t.Fatalf("node a is %T, want *AgentNode", r.Workflow.Nodes["a"])
	}
	// The block form implies the capability it bounds. Without this the
	// seven numbers would travel on a node whose codemode is off, and
	// nothing would run under them.
	if n.Codemode != "on" {
		t.Errorf("Codemode = %q, want \"on\": the block form implies it", n.Codemode)
	}
	b := n.CodemodeBounds
	if b == nil {
		t.Fatalf("CodemodeBounds is nil — the declaration did not reach the node")
	}
	for _, c := range []struct {
		name     string
		got      int64
		declared int64
	}{
		{"tool_calls", b.ToolCalls, 256},
		{"memory", b.Memory, 536870912},
		{"script_bytes", b.ScriptBytes, 524288},
		{"result_bytes", b.ResultBytes, 2097152},
		{"tool_result_bytes", b.ToolResultBytes, 131072},
	} {
		if c.got != c.declared {
			t.Errorf("%s = %d, want the declared %d", c.name, c.got, c.declared)
		}
	}
	// The durations are parsed HERE, not in the parser, so this is also the
	// witness that the unit survived the crossing — `wall_ns` against
	// `wall_ms` is the bug the host's own field doc records.
	if b.Wall != 2*time.Minute {
		t.Errorf("Wall = %s, want 2m", b.Wall)
	}
	if b.CPU != 45*time.Second {
		t.Errorf("CPU = %s, want 45s", b.CPU)
	}
}

func TestTheShortFormStillCarriesNoBounds(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode: on

workflow w:
  entry: a
  a -> done
`)
	if len(r.Diagnostics) > 0 {
		t.Fatalf("`codemode: on` drew diagnostics: %+v", r.Diagnostics)
	}
	n := r.Workflow.Nodes["a"].(*AgentNode)
	if n.Codemode != "on" {
		t.Errorf("Codemode = %q, want \"on\" — the short form must keep working untouched", n.Codemode)
	}
	// An empty bound set here would be a declaration an author never made,
	// and it would travel on the wire as one.
	if n.CodemodeBounds != nil {
		t.Errorf("CodemodeBounds = %+v, want nil: the short form declares no bound", n.CodemodeBounds)
	}
}

func TestTheWorkflowRungCarriesBoundsToo(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"

workflow w:
  entry: a
  codemode:
    tool_calls: 512
  a -> done
`)
	if len(r.Diagnostics) > 0 {
		t.Fatalf("a workflow-level block drew diagnostics: %+v", r.Diagnostics)
	}
	if r.Workflow.Codemode != "on" {
		t.Errorf("workflow Codemode = %q, want \"on\"", r.Workflow.Codemode)
	}
	if r.Workflow.CodemodeBounds == nil || r.Workflow.CodemodeBounds.ToolCalls != 512 {
		t.Errorf("workflow CodemodeBounds = %+v, want ToolCalls 512", r.Workflow.CodemodeBounds)
	}
}

func TestABoundAboveTheCeilingWarnsAndIsKept(t *testing.T) {
	t.Parallel()
	// 4097 is one past scripthost.Ceiling().ToolCalls.
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    tool_calls: 4097

workflow w:
  entry: a
  a -> done
`)
	if got := countCode(r, DiagCodemodeBoundAboveCeiling); got != 1 {
		t.Fatalf("C347 on tool_calls above the ceiling: %d time(s), want 1: %+v", got, r.Diagnostics)
	}
	for _, d := range r.Diagnostics {
		if d.Code == DiagCodemodeBoundAboveCeiling && d.Severity == SeverityError {
			t.Errorf("C347 is an ERROR: philosophy 1 warns rather than rejects, and the clamp is what enforces the bound")
		}
	}
	// KEPT, not repaired. The clamp in the parent is the single owner of the
	// enforcement; a compiler that also lowered the value would make the
	// bound a thing two places decide, and the OnClamp report would stop
	// firing on exactly the case it exists for.
	n := r.Workflow.Nodes["a"].(*AgentNode)
	if n.CodemodeBounds == nil || n.CodemodeBounds.ToolCalls != 4097 {
		t.Errorf("CodemodeBounds = %+v, want the declared 4097 carried through: the compiler warns, the parent clamps", n.CodemodeBounds)
	}
}

func TestBoundsBesideModeOffAreReported(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    mode: off
    tool_calls: 256

workflow w:
  entry: a
  a -> done
`)
	if got := countCode(r, DiagCodemodeBoundsWithModeOff); got != 1 {
		t.Fatalf("C348 on bounds beside mode: off: %d time(s), want 1: %+v", got, r.Diagnostics)
	}
	n := r.Workflow.Nodes["a"].(*AgentNode)
	if n.Codemode != "off" {
		t.Errorf("Codemode = %q, want \"off\": an explicit mode beats the block's implication", n.Codemode)
	}
}

func TestADurationThatIsNotOneIsReportedAndFallsBackToTheDefault(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    wall: "two minutes"
    cpu: "-30s"

workflow w:
  entry: a
  a -> done
`)
	if got := countCode(r, DiagCodemodeBoundUnreadable); got != 2 {
		t.Fatalf("C349 on an unreadable and a negative duration: %d time(s), want 2: %+v", got, r.Diagnostics)
	}
	n := r.Workflow.Nodes["a"].(*AgentNode)
	if n.CodemodeBounds == nil {
		t.Fatalf("the block declared two bounds; CodemodeBounds is nil")
	}
	// Zero, so Clamp fills it from DefaultLimits: tighter than whatever the
	// author meant, never looser, and the diagnostic says so.
	if n.CodemodeBounds.Wall != 0 || n.CodemodeBounds.CPU != 0 {
		t.Errorf("Wall/CPU = %s/%s, want 0/0 so the default applies", n.CodemodeBounds.Wall, n.CodemodeBounds.CPU)
	}
}

// A plain `codemode: off` draws NO C348 — the half of that diagnostic the
// first round left unwitnessed. Measured: dropping the `bounds != nil` half
// of its guard survived ./pkg/dsl/..., so every `codemode: off` in the
// language would have drawn a spurious warning with nothing to say so. In a
// tool that blocks, a false positive costs as much as a hole.
func TestAPlainCodemodeOffDrawsNoBoundsDiagnostic(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode: off

workflow w:
  entry: a
  a -> done
`)
	if got := countCode(r, DiagCodemodeBoundsWithModeOff); got != 0 {
		t.Errorf("C348 on a plain `codemode: off`: %d time(s), want 0: %+v", got, r.Diagnostics)
	}
	if len(r.Diagnostics) > 0 {
		t.Errorf("`codemode: off` drew diagnostics: %+v", r.Diagnostics)
	}
}

// Each C349 names the property it is about. Measured as unwitnessed: giving
// the cpu message the label "wall" survived ./pkg/dsl/..., because the
// sibling test counts two C349s and checks both fields are zero, never which
// message names which field. An author with two bad durations would have been
// told the same thing twice.
func TestEachUnreadableDurationNamesItsOwnProperty(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    wall: "two minutes"
    cpu: "forty"

workflow w:
  entry: a
  a -> done
`)
	var sawWall, sawCPU bool
	for _, d := range r.Diagnostics {
		if d.Code != DiagCodemodeBoundUnreadable {
			continue
		}
		if strings.Contains(d.Message, `codemode wall: "two minutes"`) {
			sawWall = true
		}
		if strings.Contains(d.Message, `codemode cpu: "forty"`) {
			sawCPU = true
		}
	}
	if !sawWall {
		t.Errorf("no C349 names wall with its own value: %+v", r.Diagnostics)
	}
	if !sawCPU {
		t.Errorf("no C349 names cpu with its own value: %+v", r.Diagnostics)
	}
}

// The judge host carries its bounds too. Measured as unwitnessed: setting the
// judge's CodemodeBounds to nil at the compile site survived ./pkg/dsl/...,
// because both compile tests used an agent and the every-kind fixture gives
// the judge only the short form. A new state honoured at one of two sites.
func TestAJudgeCarriesItsDeclaredBounds(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
judge j:
  model: "m"
  backend: "claw"
  codemode:
    tool_calls: 77

workflow w:
  entry: j
  j -> done
`)
	if len(r.Diagnostics) > 0 {
		t.Fatalf("drew diagnostics: %+v", r.Diagnostics)
	}
	n, ok := r.Workflow.Nodes["j"].(*JudgeNode)
	if !ok {
		t.Fatalf("node j is %T", r.Workflow.Nodes["j"])
	}
	if n.Codemode != "on" {
		t.Errorf("Codemode = %q, want \"on\"", n.Codemode)
	}
	if n.CodemodeBounds == nil || n.CodemodeBounds.ToolCalls != 77 {
		t.Errorf("CodemodeBounds = %+v, want ToolCalls 77 on the judge", n.CodemodeBounds)
	}
}

// The C349 gate — the fix round 2 led with, and which had no test. Measured
// as unwitnessed: replacing the gate with the pre-fix ungated form left all
// sixteen `pkg/dsl/...` packages green, because no test put a bad duration
// beside a node-rung `mode: off` (the C348 tests use `tool_calls`, the C349
// tests have no `mode: off`), so neither half of the split was observed.
func TestAnInertBlockReportsNeitherItsCeilingNorItsDurations(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    mode: off
    tool_calls: 4097
    wall: "two minutes"

workflow w:
  entry: a
  a -> done
`)
	// C348 says no program runs; a C347 about a clamp at a spawn that will
	// not happen, or a C349 about a default that will not apply, are both
	// text about an enforcement there is none of.
	if got := countCode(r, DiagCodemodeBoundsWithModeOff); got != 1 {
		t.Errorf("C348 = %d, want 1: %+v", got, r.Diagnostics)
	}
	if got := countCode(r, DiagCodemodeBoundAboveCeiling); got != 0 {
		t.Errorf("C347 = %d, want 0 on an inert block", got)
	}
	if got := countCode(r, DiagCodemodeBoundUnreadable); got != 0 {
		t.Errorf("C349 = %d, want 0 on an inert block — the gate is honoured at one of its two sites otherwise", got)
	}
	// And the parse still happened: the inert arm must agree with the
	// reporting arm on the value, only not on the diagnostics.
	n := r.Workflow.Nodes["a"].(*AgentNode)
	if n.CodemodeBounds == nil || n.CodemodeBounds.Wall != 0 {
		t.Errorf("bounds = %+v, want Wall 0 (the duration was not readable)", n.CodemodeBounds)
	}
}

// The default a refused duration falls back to is READ from the table that
// defines it, not hand-copied. Three copies of two numbers existed, and one
// of them named the wall's default in a `cpu:` message.
func TestTheRefusedDurationMessageNamesItsOwnDefaultFromTheTable(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    wall: "two minutes"
    cpu: "forty"

workflow w:
  entry: a
  a -> done
`)
	def := scripthost.DefaultLimits()
	var sawWall, sawCPU bool
	for _, d := range r.Diagnostics {
		if d.Code != DiagCodemodeBoundUnreadable {
			continue
		}
		if strings.Contains(d.Message, "codemode wall:") {
			sawWall = true
			if !strings.Contains(d.Message, "wall "+def.Wall.String()) {
				t.Errorf("the wall message does not name DefaultLimits().Wall (%s): %s", def.Wall, d.Message)
			}
		}
		if strings.Contains(d.Message, "codemode cpu:") {
			sawCPU = true
			if !strings.Contains(d.Message, "cpu "+def.CPU.String()) {
				t.Errorf("the cpu message does not name DefaultLimits().CPU (%s) — the first draft named the wall's: %s", def.CPU, d.Message)
			}
		}
	}
	if !sawWall || !sawCPU {
		t.Fatalf("expected one C349 per bad duration: %+v", r.Diagnostics)
	}
}

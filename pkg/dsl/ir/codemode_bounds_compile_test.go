package ir

import (
	"testing"
	"time"
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

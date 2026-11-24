package ir

import (
	"testing"
	"time"
)

// The workflow rung's bounds are a DEFAULT, not dead text — which is why
// C348 belongs to the node rung alone.
//
// `codemode:` is two ladders by design: ResolveCodemode for the mode and
// ResolveCodemodeBounds for the bounds, each walked separately. So a workflow
// that says "off by default, and 256 calls for whoever turns it on" is a
// coherent declaration, and the ordinary shape of a bot with one codemode
// node. A diagnostic calling those bounds inert is false, and both of its
// remedies change behaviour: dropping the bounds moves every on-node from the
// declared bound to DefaultLimits — the silent re-making of an operator's
// choice that pkg/queue's own v32 rationale calls the correctness bug the
// additive-field rule exists for — and dropping `mode: off` turns codemode on
// for the whole bot.
func TestTheWorkflowRungsBoundsAreADefaultNotDeadText(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode: on

workflow w:
  entry: a
  codemode:
    mode: off
    tool_calls: 256
    wall: "9m"
  a -> done
`)
	// No C348: the bounds are reachable, so saying they are not would be a
	// false statement with a behaviour-changing remedy.
	if got := countCode(r, DiagCodemodeBoundsWithModeOff); got != 0 {
		t.Errorf("C348 at the workflow rung: %d time(s), want 0 — the bounds are the default every on-node runs under: %+v", got, r.Diagnostics)
	}
	if r.Workflow.Codemode != "off" {
		t.Fatalf("workflow Codemode = %q, want \"off\"", r.Workflow.Codemode)
	}

	// The two ladders, walked the way the executor walks them
	// (executor_codemode.go): the mode from the node, the bounds from
	// whichever rung set each field.
	n := r.Workflow.Nodes["a"].(*AgentNode)
	policy := ResolveCodemode("", n.GetCodemode(), r.Workflow.Codemode)
	if !policy.On {
		t.Fatalf("policy = %s, want on: an explicit node value beats the workflow's", policy)
	}
	bounds := ResolveCodemodeBounds(nil, n.CodemodeBounds, r.Workflow.CodemodeBounds)
	if bounds == nil {
		t.Fatalf("the node resolved no bounds, so the workflow's declaration reached nothing")
	}
	if bounds.ToolCalls != 256 {
		t.Errorf("ToolCalls = %d, want the workflow's 256 — this is the number C348 called inert", bounds.ToolCalls)
	}
	if bounds.Wall != 9*time.Minute {
		t.Errorf("Wall = %s, want the workflow's 9m", bounds.Wall)
	}
}

// And the node rung, where the same words ARE true: a node that declares
// `mode: off` is off whatever the workflow says (ResolveCodemode takes the
// nearest explicit value), so no program of that node runs and its own bounds
// bound nothing. C348 is kept here, and only here.
func TestTheNodeRungsBoundsBesideModeOffReallyAreInert(t *testing.T) {
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
  codemode: on
  a -> done
`)
	if got := countCode(r, DiagCodemodeBoundsWithModeOff); got != 1 {
		t.Fatalf("C348 at the node rung: %d time(s), want 1: %+v", got, r.Diagnostics)
	}
	n := r.Workflow.Nodes["a"].(*AgentNode)
	// The node's explicit off beats the workflow's on — which is what makes
	// its own bounds unreachable, and the diagnostic true.
	if policy := ResolveCodemode("", n.GetCodemode(), r.Workflow.Codemode); policy.On {
		t.Errorf("policy = %s, want off: an explicit node `off` beats a workflow `on`", policy)
	}
}

// The positioning contract docs/references/diagnostics.md states for any
// finding about a property's VALUE, executed rather than trusted: "points at
// that property's line in the declaration … and `iterion validate --json`
// names the property (`property`) and, on a `.bot` source, closes the span
// (`end_line`, `end_column`)".
//
// All three codemode diagnostics are about a property's value, so all three
// owe it. They used warnfAtSpan, which that helper's own doc reserves for a
// declaration that is NOT a graph node (a prompt, a schema) and which carries
// neither NodeID nor property nor a closed span — so the studio badge and the
// MCP hint, which match on the structured half, had nothing to match. The
// message text named the node, which is why a CLI reader saw nothing wrong.
func TestTheCodemodeDiagnosticsCarryTheirPropertyAndTheirNode(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    tool_calls: 4097
    wall: "two minutes"

workflow w:
  entry: a
  a -> done
`)
	seen := 0
	for _, d := range r.Diagnostics {
		switch d.Code {
		case DiagCodemodeBoundAboveCeiling, DiagCodemodeBoundUnreadable, DiagCodemodeBoundsWithModeOff:
		default:
			continue
		}
		seen++
		if d.Property != "codemode" {
			t.Errorf("%s: Property = %q, want \"codemode\"", d.Code, d.Property)
		}
		if d.NodeID != "a" {
			t.Errorf("%s: NodeID = %q, want \"a\"", d.Code, d.NodeID)
		}
		if d.Line == 0 {
			t.Errorf("%s: no line", d.Code)
		}
		if d.EndLine == 0 || d.EndColumn == 0 {
			t.Errorf("%s: span not closed (end %d:%d) — a `.bot` source owes end_line/end_column", d.Code, d.EndLine, d.EndColumn)
		}
	}
	if seen != 2 {
		t.Fatalf("expected one C347 and one C349 on this block, saw %d codemode diagnostics: %+v", seen, r.Diagnostics)
	}
}

func TestCodemodeStoreQuotasResolveAtRunScope(t *testing.T) {
	t.Parallel()
	assert := func(run, workflow *CodemodeBounds, wantEntries, wantBytes int64) {
		t.Helper()
		entries, bytes := ResolveCodemodeStoreLimits(run, workflow)
		if entries != wantEntries || bytes != wantBytes {
			t.Fatalf("bank limits = %d, %d; want %d, %d", entries, bytes, wantEntries, wantBytes)
		}
	}
	assert(nil, nil, 32, 128<<10)
	workflow := &CodemodeBounds{StoreEntries: 48, StoreBytes: 256 << 10}
	assert(&CodemodeBounds{}, workflow, 48, 256<<10)
	assert(&CodemodeBounds{StoreEntries: 12}, workflow, 12, 256<<10)
	assert(&CodemodeBounds{StoreBytes: 640 << 10}, workflow, 48, 512<<10)
	assert(&CodemodeBounds{StoreEntries: 100}, nil, 64, 128<<10)
	program := ResolveCodemodeBounds(workflow, workflow, workflow)
	if program.StoreEntries != 0 || program.StoreBytes != 0 {
		t.Fatalf("per-program resolver returned run-wide limits: %+v", program)
	}
}

func TestCodemodeReplayRetainsOmittedBankQuotas(t *testing.T) {
	t.Parallel()
	prior := &CodemodeBounds{ToolCalls: 200, ResultBytes: 1024, StoreEntries: 48, StoreBytes: 256 << 10}
	for _, row := range []struct{ ask, want *CodemodeBounds }{
		{nil, prior},
		{&CodemodeBounds{StoreEntries: 16}, &CodemodeBounds{ToolCalls: 200, ResultBytes: 1024, StoreEntries: 16, StoreBytes: 256 << 10}},
		{&CodemodeBounds{ToolCalls: 100}, &CodemodeBounds{ToolCalls: 100, StoreEntries: 48, StoreBytes: 256 << 10}},
		{&CodemodeBounds{ToolCalls: 100, StoreBytes: 512 << 10}, &CodemodeBounds{ToolCalls: 100, StoreEntries: 48, StoreBytes: 512 << 10}},
	} {
		if got := ReplayCodemodeBounds(row.ask, prior); *got != *row.want {
			t.Errorf("restatement %+v resolved %+v, want %+v", row.ask, got, row.want)
		}
	}
}

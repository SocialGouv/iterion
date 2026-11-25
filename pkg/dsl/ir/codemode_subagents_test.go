package ir

import (
	"testing"
)

// The subagents reserve (CM-13): the DSL compiles it onto all three rungs,
// refuses a reserve that cannot arm, warns where it is inert, and the
// unparser round-trips every key. A dropped reserve would silently keep the
// agent tool withheld from a node whose author sized it.

func TestTheSubagentsReserveCompilesOntoEveryRung(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    subagents_tokens: 100000
    subagents_tokens_per_agent: 40000
    subagents_concurrent: 2
    subagents_steps_per_agent: 8

workflow w:
  entry: a
  codemode:
    subagents_tokens: 500000
  a -> done
`)
	if len(r.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", r.Diagnostics)
	}
	wf := r.Workflow
	if wf.CodemodeSubagents == nil || wf.CodemodeSubagents.Tokens != 500000 {
		t.Fatalf("the workflow rung lost its reserve: %+v", wf.CodemodeSubagents)
	}
	agent := wf.Nodes["a"].(*AgentNode)
	if agent.CodemodeSubagents == nil {
		t.Fatal("the node rung lost its reserve")
	}
	if agent.CodemodeSubagents.Tokens != 100000 ||
		agent.CodemodeSubagents.TokensPerAgent != 40000 ||
		agent.CodemodeSubagents.Concurrent != 2 ||
		agent.CodemodeSubagents.StepsPerAgent != 8 {
		t.Fatalf("the node's reserve is not what the block declared: %+v", agent.CodemodeSubagents)
	}
}

// The cascade is field by field, like the bounds: a workflow that sizes a
// whole reserve is a DEFAULT a node may raise or tighten one field of.
func TestTheSubagentsReserveCascadeIsFieldByField(t *testing.T) {
	t.Parallel()
	wf := &CodemodeSubagents{Tokens: 500000, TokensPerAgent: 100000, Concurrent: 2}
	node := &CodemodeSubagents{Tokens: 80000}
	got := ResolveCodemodeSubagents(nil, node, wf)
	if got == nil {
		t.Fatal("a node-declared field must arm the reserve")
	}
	if got.Tokens != 80000 || got.TokensPerAgent != 100000 || got.Concurrent != 2 {
		t.Fatalf("field-by-field resolution broken: %+v", got)
	}
	// The arming rule: no rung sized a reserve, nothing is armed — however
	// many other fields the rungs declared.
	if ResolveCodemodeSubagents(nil, &CodemodeSubagents{Concurrent: 3}, &CodemodeSubagents{StepsPerAgent: 8}) != nil {
		t.Fatal("a reserve no rung sized must stay unarmed")
	}
	if ResolveCodemodeSubagents(nil, nil, nil) != nil {
		t.Fatal("nothing declared anywhere must stay unarmed")
	}
}

// C356 judges the declaration against the size the CASCADE would give it:
// a node that tightens one field under a workflow-sized reserve compiles
// clean, and the cascade fills the size. Red on the first draft, whose
// per-rung check refused exactly that node.
func TestANodeTighteningOneFieldUnderAWorkflowReserveCompiles(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    subagents_concurrent: 1

workflow w:
  entry: a
  codemode:
    subagents_tokens: 100000
  a -> done
`)
	if got := countCode(r, DiagCodemodeSubagentsUnsized); got != 0 {
		t.Fatalf("C356 fired on a field-by-field narrowing: %d time(s): %+v", got, r.Diagnostics)
	}
	agent := r.Workflow.Nodes["a"].(*AgentNode)
	if agent.CodemodeSubagents == nil || agent.CodemodeSubagents.Concurrent != 1 {
		t.Fatalf("the node's declared field was lost: %+v", agent.CodemodeSubagents)
	}
}

// C356, both halves: an unsized reserve and a hold wider than its reserve
// are errors that arm nothing.
func TestAnUnsizedOrOverheldReserveIsAnError(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    subagents_concurrent: 3

workflow w:
  entry: a
  a -> done
`)
	if got := countCode(r, DiagCodemodeSubagentsUnsized); got != 1 {
		t.Fatalf("C356 on an unsized reserve: %d time(s), want 1: %+v", got, r.Diagnostics)
	}
	if r.Workflow.Nodes["a"].(*AgentNode).CodemodeSubagents != nil {
		t.Fatal("an unsized reserve must arm nothing")
	}

	r2 := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    subagents_tokens: 1000
    subagents_tokens_per_agent: 2000

workflow w:
  entry: a
  a -> done
`)
	if got := countCode(r2, DiagCodemodeSubagentsUnsized); got != 1 {
		t.Fatalf("C356 on a hold above its reserve: %d time(s), want 1: %+v", got, r2.Diagnostics)
	}
	if r2.Workflow.Nodes["a"].(*AgentNode).CodemodeSubagents != nil {
		t.Fatal("a hold wider than its reserve must arm nothing")
	}
}

// C357: the reserve beside `mode: off` on a node arms nothing as written,
// and the warning says the launch reach like C348 and C355 do.
func TestAReserveBesideModeOffWarnsAndArmsNothing(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    mode: off
    subagents_tokens: 1000

workflow w:
  entry: a
  a -> done
`)
	if got := countCode(r, DiagCodemodeSubagentsWithModeOff); got != 1 {
		t.Fatalf("C357 beside mode: off: %d time(s), want 1: %+v", got, r.Diagnostics)
	}
	// As WRITTEN the node's codemode is off and its own reserve is inert;
	// the compiled node still carries the declaration, the way C348's
	// bounds do — the launch override reads it.
	if a := r.Workflow.Nodes["a"].(*AgentNode); a.CodemodeSubagents == nil || a.CodemodeSubagents.Tokens != 1000 {
		t.Fatalf("the declared reserve must be kept as written: %+v", a.CodemodeSubagents)
	}
}

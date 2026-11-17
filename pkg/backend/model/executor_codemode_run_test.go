package model

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The run-level codemode override (CM-5e, #3345) reaches codemodePolicy
// through the executor fields WithCodemodeOverride / WithCodemodeBoundsOverride
// set. These witnesses bite the two directions a reader can go deaf: a mode
// the dispatch decides over the operator's head, and a bound the launch chose
// that the resolver drops.

// TestARunOverrideDecidesTheExecutorPolicy — the run rung is nearest for the
// MODE: an operator's off keeps a declared capability away, an operator's on
// arms a node whose author stayed silent (and is REFUSED on a backend that
// does not serve it — a launch choice is a declaration, and the refusal is
// terminal; that direction is the screen's job upstream of the run).
func TestARunOverrideDecidesTheExecutorPolicy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		run      string
		node     string
		workflow string
		wantOn   bool
		wantSrc  string
	}{
		{"launch off kills a declared on", "off", "on", "", false, ir.CodemodeFromRun},
		{"launch on arms a silent node", "on", "", "", true, ir.CodemodeFromRun},
		{"launch on beats an authored off", "on", "off", "", true, ir.CodemodeFromRun},
		{"empty run inherits the node", "", "on", "off", true, ir.CodemodeFromNode},
		{"empty run inherits the workflow", "", "", "on", true, ir.CodemodeFromWorkflow},
		{"nobody chose: the default", "", "", "", false, ir.CodemodeFromDefault},
	}
	for _, c := range cases {
		e := &ClawExecutor{wfCodemode: c.workflow, codemodeOverride: c.run}
		got, _, err := e.codemodePolicy(backendFields{id: "n", codemode: c.node}, backendcaps.Claw)
		if err != nil {
			t.Fatalf("%s: codemodePolicy returned %v", c.name, err)
		}
		if got.On != c.wantOn || got.Source != c.wantSrc {
			t.Errorf("%s: policy = %+v, want On=%v from %s", c.name, got, c.wantOn, c.wantSrc)
		}
		// bounds MAY be nil on an on policy: nobody chose bounds, and nil is
		// what installCodemode reads as "run under the defaults". The ladder
		// decides the values, not the presence.
	}
}

// TestARunBoundsOverrideBeatsTheAuthoredRungsFieldByField — the bound the
// launch set wins; the fields it left at zero keep the authored values, node
// then workflow. All seven fields carry distinct values on every rung: one
// deaf field in the resolver would otherwise survive this witness the way it
// survived the single-field first draft (found by the adversarial round — a
// mutant dropping Wall alone stayed green everywhere).
func TestARunBoundsOverrideBeatsTheAuthoredRungsFieldByField(t *testing.T) {
	t.Parallel()
	run := &ir.CodemodeBounds{
		ToolCalls: 4096, Wall: 91 * time.Second, CPU: 31 * time.Second,
		Memory: 1 << 30, ScriptBytes: 1 << 20, ResultBytes: 2 << 20,
		ToolResultBytes: 512 << 10,
	}
	node := &ir.CodemodeBounds{
		ToolCalls: 64, Wall: 30 * time.Second, CPU: 20 * time.Second,
		Memory: 256 << 20, ScriptBytes: 512 << 10, ResultBytes: 1 << 20,
		ToolResultBytes: 128 << 10,
	}
	e := &ClawExecutor{
		codemodeOverride:       "on",
		codemodeBoundsOverride: run,
		wfCodemodeBounds:       &ir.CodemodeBounds{ToolCalls: 128, CPU: 40 * time.Second},
	}
	_, bounds, err := e.codemodePolicy(backendFields{id: "n", codemode: "on", codemodeBounds: node}, backendcaps.Claw)
	if err != nil {
		t.Fatalf("codemodePolicy returned %v", err)
	}
	if bounds == nil {
		t.Fatal("bounds resolved nil with a launch bound set")
	}
	for _, row := range []struct {
		name      string
		got, want any
	}{
		{"tool_calls", bounds.ToolCalls, run.ToolCalls},
		{"wall", bounds.Wall, run.Wall},
		{"cpu", bounds.CPU, run.CPU},
		{"memory", bounds.Memory, run.Memory},
		{"script_bytes", bounds.ScriptBytes, run.ScriptBytes},
		{"result_bytes", bounds.ResultBytes, run.ResultBytes},
		{"tool_result_bytes", bounds.ToolResultBytes, run.ToolResultBytes},
	} {
		if row.got != row.want {
			t.Errorf("%s = %v, want the launch's %v — the run rung went deaf on this field", row.name, row.got, row.want)
		}
	}
}

// TestARunOffOverrideCarriesNoBounds — an off policy returns before the
// bounds ladder is walked, run override included: a non-nil bound set beside a
// Codemode:false task would read as a budget nobody can spend. The executor
// may hold a bounds override (the operator also set flags) while asking for
// off; the pair must still come back nil.
func TestARunOffOverrideCarriesNoBounds(t *testing.T) {
	t.Parallel()
	e := &ClawExecutor{
		wfCodemode:             "on",
		codemodeOverride:       "off",
		codemodeBoundsOverride: &ir.CodemodeBounds{ToolCalls: 4096},
	}
	policy, bounds, err := e.codemodePolicy(backendFields{id: "n"}, backendcaps.Claw)
	if err != nil {
		t.Fatalf("codemodePolicy returned %v", err)
	}
	if policy.On || bounds != nil {
		t.Errorf("policy %+v bounds %+v, want off and nil — the operator asked the capability away", policy, bounds)
	}
}

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
// then workflow. Measured on ToolCalls (the bound the CM-17 measurement saw
// bite) and Wall/CPU as the inherit direction.
func TestARunBoundsOverrideBeatsTheAuthoredRungsFieldByField(t *testing.T) {
	t.Parallel()
	e := &ClawExecutor{
		codemodeOverride:       "on",
		codemodeBoundsOverride: &ir.CodemodeBounds{ToolCalls: 4096},
		wfCodemodeBounds:       &ir.CodemodeBounds{ToolCalls: 128, CPU: 20 * time.Second},
	}
	_, bounds, err := e.codemodePolicy(backendFields{
		id:             "n",
		codemode:       "on",
		codemodeBounds: &ir.CodemodeBounds{ToolCalls: 64, Wall: 30 * time.Second},
	}, backendcaps.Claw)
	if err != nil {
		t.Fatalf("codemodePolicy returned %v", err)
	}
	if bounds == nil {
		t.Fatal("bounds resolved nil with a launch bound set")
	}
	if bounds.ToolCalls != 4096 {
		t.Errorf("ToolCalls = %d, want the launch's 4096", bounds.ToolCalls)
	}
	if bounds.Wall != 30*time.Second {
		t.Errorf("Wall = %s, want the node's 30s (the launch did not choose)", bounds.Wall)
	}
	if bounds.CPU != 20*time.Second {
		t.Errorf("CPU = %s, want the workflow's 20s", bounds.CPU)
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

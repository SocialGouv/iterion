package ir

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
)

// The run rung (CM-5e, #3345): the launch request's explicit choice is the
// NEAREST rung of both ladders — the mode and, field by field, the bounds.
// The failure direction these witnesses bite is a rung gone deaf: a resolver
// that ignores its run argument silently runs the authored policy over the
// operator's launch choice. That is the "knob failing open" the queue's
// v-version history records for every launch-time override before this one.

// TestTheRunRungDecidesTheMode — a launch override wins over both authored
// rungs in BOTH directions, and "" inherits the authored ladder untouched.
func TestTheRunRungDecidesTheMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		run      string
		node     string
		workflow string
		want     CodemodePolicy
	}{
		{"launch off kills a declared on", "off", "on", "on", CodemodePolicy{On: false, Source: CodemodeFromRun}},
		{"launch on arms a silent node", "on", "", "", CodemodePolicy{On: true, Source: CodemodeFromRun}},
		{"launch on beats an authored off", "on", "off", "", CodemodePolicy{On: true, Source: CodemodeFromRun}},
		{"empty run inherits the node", "", "on", "off", CodemodePolicy{On: true, Source: CodemodeFromNode}},
		{"empty run inherits the workflow", "", "", "off", CodemodePolicy{On: false, Source: CodemodeFromWorkflow}},
		{"empty everywhere stays the default", "", "", "", CodemodePolicy{On: false, Source: CodemodeFromDefault}},
		{"launch beats BOTH authored rungs", "off", "on", "on", CodemodePolicy{On: false, Source: CodemodeFromRun}},
	}
	for _, c := range cases {
		got := ResolveCodemode(c.run, c.node, c.workflow)
		if got != c.want {
			t.Errorf("%s: ResolveCodemode(%q, %q, %q) = %+v, want %+v", c.name, c.run, c.node, c.workflow, got, c.want)
		}
	}
}

// TestTheRunRungDecidesTheBoundsFieldByField — the launch rung follows the
// ONE rule the other two rungs already follow: a field the operator set wins,
// every field they left at zero keeps the authored value, and an entirely
// empty launch rung changes nothing (nil stays nil — nobody chose).
func TestTheRunRungDecidesTheBoundsFieldByField(t *testing.T) {
	t.Parallel()
	// EVERY field at the run rung, each with its own value on all three
	// rungs: one deaf pick closure for ONE field would otherwise stay green
	// (the launch-time raise is the exact shape the CM-17 measurement saw
	// the model ask the operator for — a deaf rung drops it in silence).
	runValues := CodemodeBounds{
		ToolCalls: 4096, Wall: 91 * time.Second, CPU: 31 * time.Second,
		Memory: 1 << 30, ScriptBytes: 1 << 20, ResultBytes: 2 << 20,
		ToolResultBytes: 512 << 10, ConcurrentToolCalls: 1,
	}
	nodeValues := CodemodeBounds{
		ToolCalls: 64, Wall: 30 * time.Second, CPU: 20 * time.Second,
		Memory: 256 << 20, ScriptBytes: 512 << 10, ResultBytes: 1 << 20,
		ToolResultBytes: 128 << 10, ConcurrentToolCalls: 3,
	}
	workflowValues := CodemodeBounds{
		ToolCalls: 128, Wall: 60 * time.Second, CPU: 40 * time.Second,
		Memory: 512 << 20, ScriptBytes: 256 << 10, ResultBytes: 512 << 10,
		ToolResultBytes: 64 << 10, ConcurrentToolCalls: 4,
	}
	got := ResolveCodemodeBounds(&runValues, &nodeValues, &workflowValues)
	if got == nil {
		t.Fatal("a run bound set beside authored bounds resolved nil — the operator's raise was dropped")
	}
	for _, row := range []struct {
		name      string
		got, want any
	}{
		{"tool_calls", got.ToolCalls, runValues.ToolCalls},
		{"concurrent_tool_calls", got.ConcurrentToolCalls, runValues.ConcurrentToolCalls},
		{"wall", got.Wall, runValues.Wall},
		{"cpu", got.CPU, runValues.CPU},
		{"memory", got.Memory, runValues.Memory},
		{"script_bytes", got.ScriptBytes, runValues.ScriptBytes},
		{"result_bytes", got.ResultBytes, runValues.ResultBytes},
		{"tool_result_bytes", got.ToolResultBytes, runValues.ToolResultBytes},
	} {
		if row.got != row.want {
			t.Errorf("%s = %v, want the launch's %v — the run rung went deaf on this field", row.name, row.got, row.want)
		}
	}

	// Fields the launch left at zero keep the authored values, nearest first.
	partial := ResolveCodemodeBounds(&CodemodeBounds{ToolCalls: 4096}, &nodeValues, &workflowValues)
	if partial == nil {
		t.Fatal("a partial run bound resolved nil")
	}
	if partial.ConcurrentToolCalls != 3 {
		t.Errorf("ConcurrentToolCalls = %d, want the node's 3 (the launch did not choose)", partial.ConcurrentToolCalls)
	}
	if got := ResolveCodemodeBounds(nil, &CodemodeBounds{}, &workflowValues); got == nil || got.ConcurrentToolCalls != 4 {
		t.Errorf("ResolveCodemodeBounds with no run or node concurrency = %+v, want the workflow's 4", got)
	}
	if partial.ToolCalls != 4096 {
		t.Errorf("ToolCalls = %d, want the launch's 4096", partial.ToolCalls)
	}
	if partial.Wall != 30*time.Second {
		t.Errorf("Wall = %s, want the node's 30s (the launch did not choose)", partial.Wall)
	}
	if partial.CPU != 20*time.Second {
		t.Errorf("CPU = %s, want the node's 20s (the launch did not choose)", partial.CPU)
	}
	if partial.Memory != 256<<20 {
		t.Errorf("Memory = %d, want the node's 256 MiB", partial.Memory)
	}
	if partial.CPU == 40*time.Second {
		t.Errorf("CPU fell through to the workflow — the node rung is nearer")
	}

	// A launch bound set beside NO authored bound still lands.
	if got := ResolveCodemodeBounds(&CodemodeBounds{ToolCalls: 4096}, nil, nil); got == nil || got.ToolCalls != 4096 {
		t.Errorf("ResolveCodemodeBounds(run, nil, nil) = %+v, want ToolCalls 4096", got)
	}
	// Nobody chose anywhere: nil, not a zero struct that would read as a
	// declaration of defaults.
	if got := ResolveCodemodeBounds(nil, nil, nil); got != nil {
		t.Errorf("ResolveCodemodeBounds(nil, nil, nil) = %+v, want nil", got)
	}
}

// TestTheScreenSeesTheRunRung — the picker's screen answers what the DISPATCH
// will answer, and the dispatch grants on the launch's choice too: an
// explicit launch `on` on a node whose author stayed silent is refused on a
// backend that serves no codemode tool, and an explicit launch `off` on a
// declared node is refused nowhere.
func TestTheScreenSeesTheRunRung(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent silent:
  model: "m"

agent declared:
  model: "m"
  codemode: off

workflow w:
  entry: silent
  silent -> declared
  declared -> done
`)
	wf := r.Workflow
	if wf == nil {
		t.Fatalf("the fixture did not compile: %+v", r.Diagnostics)
	}
	var refused bool
	for _, backend := range backendcaps.Names() {
		if CodemodeServedBy(backend) {
			continue
		}
		refused = true
		// Launch on, author silent: the dispatch WILL refuse this (the run
		// rung is nearest, a launch choice is a declaration), so the screen
		// must name it before the operator pays.
		if got := CodemodeOverrideRefusalReason(backend, "on", wf.Nodes["silent"].(LLMNode), wf); got == "" {
			t.Errorf("backend %q: the screen stays silent on a launch override the dispatch will refuse", backend)
		}
		break
	}
	if !refused {
		t.Fatal("the registry gave no refusing backend; this arm needs one")
	}
	// Launch off on a refusing backend: nothing is refused — the operator's
	// off keeps the tool away and the run on route.
	if got := CodemodeOverrideRefusalReason(backendcaps.Codex, "off", wf.Nodes["silent"].(LLMNode), wf); got != "" {
		t.Errorf("a launch off screens a refusal on %s: %q", backendcaps.Codex, got)
	}
}

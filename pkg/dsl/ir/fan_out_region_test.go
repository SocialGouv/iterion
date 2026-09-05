package ir_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// sharedTargetSource is the mono/dual topology review-pr and evolve ship: a
// condition router reaches reviewer `a` directly (mono) or through a
// fan_out_all (dual). `a` has two predecessors, only one inside the fan-out.
// `tail` sits AFTER the declared collector, on the trunk.
const sharedTargetSource = `
vars:
  dual: bool = false

schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent start:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router topology:
  mode: condition

router fan:
  mode: fan_out_all

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent b:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent merge:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  await: best_effort

agent tail:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  session: persist

workflow shared_target:
  entry: start
  start -> topology
  topology -> fan when "vars.dual"
  topology -> a else
  fan -> a
  fan -> b
  a -> merge
  b -> merge
  merge -> tail
  tail -> done
`

func compileSharedTarget(t *testing.T) *ir.CompileResult {
	t.Helper()
	pr := parser.Parse("shared_target.bot", sharedTargetSource)
	if pr.File == nil {
		for _, d := range pr.Diagnostics {
			t.Logf("parse: %s", d.Error())
		}
		t.Fatal("parse returned no AST")
	}
	return ir.Compile(pr.File)
}

// The compiler's ownership view stops at the node the runtime stops at. A
// fan-out target with a trunk predecessor is not that node, so the trunk
// tail after the collector is not a branch body — and `session: persist`
// there is legal (C243 is for nodes INSIDE a fan-out body).
func TestSharedTargetFanOut_TrunkTailIsNotABranchBody(t *testing.T) {
	cr := compileSharedTarget(t)
	for _, d := range cr.Diagnostics {
		if d.Code == ir.DiagPersistInFanOut {
			t.Fatalf("C243 fired on the trunk tail: %s", d.Error())
		}
		if strings.Contains(d.Error(), "tail") && d.Severity == ir.SeverityError {
			t.Fatalf("unexpected error on the trunk tail: %s", d.Error())
		}
	}
	if cr.HasErrors() {
		for _, d := range cr.Diagnostics {
			t.Logf("diag: %s", d.Error())
		}
		t.Fatal("compile errors")
	}
}

func edgesFrom(w *ir.Workflow, from string) []*ir.Edge {
	var out []*ir.Edge
	for _, e := range w.Edges {
		if e.From == from {
			out = append(out, e)
		}
	}
	return out
}

// The election shared by the runtime and the compiler: the fan-out's
// branches reconverge at the declared collector, never at a target that
// merely has a second predecessor outside the fan-out.
func TestExecBranchConvergencePoint_SharedTargetElectsTheDeclaredCollector(t *testing.T) {
	cr := compileSharedTarget(t)
	if cr.HasErrors() {
		t.Fatal("compile errors")
	}
	if got := ir.ExecBranchConvergencePoint(cr.Workflow, "fan", edgesFrom(cr.Workflow, "fan")); got != "merge" {
		t.Errorf("collector = %q, want merge", got)
	}
	in := ir.FanOutInSources(cr.Workflow, "fan")
	if len(in["a"]) != 1 || !in["a"]["fan"] {
		t.Errorf("region sources of a = %v, want {fan} only (topology is outside the fan-out)", in["a"])
	}
	if len(in["merge"]) != 2 || !in["merge"]["a"] || !in["merge"]["b"] {
		t.Errorf("region sources of merge = %v, want {a, b}", in["merge"])
	}
}

// trunkBypassSource: a fan_out_each whose template is a linear chain, with
// the trunk bypassing the fan-out into `collect` when there is nothing to
// fan out. `collect` declares no `await:` — the bypass edge is its second
// predecessor and the only thing that elects it. `tail` is trunk-only.
const trunkBypassSource = `
schema plan_out:
  has_items: bool
  items: json

schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent plan:
  model: "m"
  input: s
  output: plan_out
  system: sys
  user: usr

router dispatch:
  mode: fan_out_each
  over: "{{outputs.plan.items}}"
  as: item

agent work:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent collect:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent tail:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  session: persist

workflow trunk_bypass:
  entry: plan
  plan -> dispatch when has_items
  plan -> collect when not has_items
  dispatch -> work
  work -> collect
  collect -> tail
  tail -> done
`

func compileTrunkBypass(t *testing.T) *ir.CompileResult {
	t.Helper()
	pr := parser.Parse("trunk_bypass.bot", trunkBypassSource)
	if pr.File == nil {
		for _, d := range pr.Diagnostics {
			t.Logf("parse: %s", d.Error())
		}
		t.Fatal("parse returned no AST")
	}
	return ir.Compile(pr.File)
}

// The bypass edge below the template head still elects the implicit
// collector, so the trunk tail after it is not a branch body: no C243 on
// `tail`, and the election names `collect`.
func TestTrunkBypassFanOutEach_ImplicitCollectorKeepsTheTrunkTailOutOfTheBody(t *testing.T) {
	cr := compileTrunkBypass(t)
	for _, d := range cr.Diagnostics {
		if d.Code == ir.DiagPersistInFanOut {
			t.Fatalf("C243 fired on the trunk tail: %s", d.Error())
		}
	}
	if cr.HasErrors() {
		for _, d := range cr.Diagnostics {
			t.Logf("diag: %s", d.Error())
		}
		t.Fatal("compile errors")
	}
	if got := ir.ExecBranchConvergencePoint(cr.Workflow, "dispatch", edgesFrom(cr.Workflow, "dispatch")); got != "collect" {
		t.Errorf("collector = %q, want collect", got)
	}
	in := ir.FanOutInSources(cr.Workflow, "dispatch")
	if len(in["collect"]) != 2 || !in["collect"]["work"] || !in["collect"]["plan"] {
		t.Errorf("sources of collect = %v, want {work, plan}: the bypass below the head counts", in["collect"])
	}
	if len(in["work"]) != 1 || !in["work"]["dispatch"] {
		t.Errorf("sources of work = %v, want {dispatch}", in["work"])
	}
}

// A predecessor reachable from the router only through a bounded back-edge
// stays outside the region: bounded iteration is local control flow.
func TestFanOutInSources_IgnoresBoundedBackEdges(t *testing.T) {
	w := &ir.Workflow{
		Nodes: map[string]ir.Node{
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"join":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitWaitAll},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
			{From: "join", To: "router", LoopName: "again"},
			{From: "join", To: "done"},
		},
	}
	in := ir.FanOutInSources(w, "router")
	if in["router"] != nil {
		t.Errorf("the trunk loop back-edge must not make the router its own predecessor, got %v", in["router"])
	}
	if len(in["join"]) != 2 {
		t.Errorf("region sources of join = %v, want {a, b}", in["join"])
	}
}

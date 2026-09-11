package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The settled floor is what a convergence node falls back on when the
// fan-out that fed it stabilized without a single branch producing output
// (#559, #1113). These tests fence what it must NOT do, which is the half
// that decides whether the fix is safe:
//
//   - it never applies an edge whose source RAN (that is routing's
//     selection to make — #484);
//   - it never applies an edge outside the invocation that settled;
//   - it never outranks a live edge or a back-edge overlay.
//
// A negative test here only bites if it names an edge that CLEARS the
// ordinary filters, so relaxing the floor is what turns it red. An edge
// edgeInIncoming already rejects would stay green through any mutation and
// prove nothing.

// settledRef builds a with-mapping reading {{outputs.<node>.<field>}}.
func settledRef(key, node, field string) *ir.DataMapping {
	raw := "{{outputs." + node + "." + field + "}}"
	return &ir.DataMapping{
		Key:  key,
		Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{node, field}, Raw: raw}},
		Raw:  raw,
	}
}

// settledLiteral builds a with-mapping carrying a constant.
func settledLiteral(key, value string) *ir.DataMapping {
	return &ir.DataMapping{Key: key, Raw: value}
}

// settledSame compares a resolved mapping against its expected value while
// tolerating the int → float64 widening a JSON checkpoint round-trip
// applies to every number, so a resume assertion is about the floor rather
// than about the encoding.
func settledSame(got, want any) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return fmt.Sprint(got) == fmt.Sprint(want)
}

// settledEntryOutput is the parent output every workflow below hands its
// fan-out: values that survive any branch failure, which is exactly why a
// mapping reading them has no business going missing.
func settledEntryOutput() map[string]any {
	return map[string]any{"summary": "go", "count": 2, "stale_count": 99, "note": "n"}
}

// ---------------------------------------------------------------------------
// Provenance: the floor is the invocation's own edges, nobody else's
// ---------------------------------------------------------------------------

// A node outside the fan-out, never reached, must stay out of the floor —
// even though it clears every ordinary filter the way the settled edges do
// (no output, join left untracked). Relaxing the floor to "any edge whose
// source has no output" is precisely what this catches.
func TestSettledFloor_ForeignEdgeIsNotSettled(t *testing.T) {
	wf := fanOutWorkflow(ir.AwaitBestEffort)
	for _, e := range wf.Edges {
		if e.To == "finalize" {
			e.With = append(e.With, settledRef("expected_count", "entry", "count"))
		}
	}
	wf.Nodes["outsider"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "outsider"}}
	// Declared LAST, so declaration order would hand it the win on the
	// shared key if it were ever applied.
	wf.Edges = append(wf.Edges, &ir.Edge{From: "outsider", To: "finalize", With: []*ir.DataMapping{
		settledRef("expected_count", "entry", "stale_count"),
		settledRef("only_outsider", "entry", "stale_count"),
	}})

	got := runSettledFanOut(t, wf, "run-559-foreign", map[string]func() (map[string]any, error){
		"agent_a": func() (map[string]any, error) { return nil, errors.New("fail A") },
		"agent_b": func() (map[string]any, error) { return nil, errors.New("fail B") },
	}, "finalize")

	if got["expected_count"] != 2 {
		t.Fatalf("expected_count = %v, want 2 (the fan-out's own edges) — %v means the outsider's edge was settled too", got["expected_count"], got["expected_count"])
	}
	if v, ok := got["only_outsider"]; ok {
		t.Fatalf("only_outsider = %v: an edge from outside the stabilized invocation reached the join", v)
	}
}

// The walk has to reach past the node the branch STARTED at: a branch that
// dies on its first node never runs the node that actually carries the edge
// into the join, so a floor derived from the branch's start node alone (or
// from what the branch recorded before failing) finds nothing.
func TestSettledFloor_WalksPastTheBranchStartNode(t *testing.T) {
	wf := settledLongBranchWorkflow()
	got := runSettledFanOut(t, wf, "run-559-long", map[string]func() (map[string]any, error){
		"a": func() (map[string]any, error) { return nil, errors.New("fail A") },
		"b": func() (map[string]any, error) { return nil, errors.New("fail B") },
	}, "join")

	if got["expected_count"] != 2 {
		t.Fatalf("input = %v — the edge into the join is carried by a2/b2, two hops past the launched edge; a floor anchored on the branch start node misses it", got)
	}
}

// Every exclusion the walk performs gets a source that is reachable ONLY
// through it, so removing that exclusion changes the result. A node the walk
// would reach anyway proves nothing about the guard that was supposed to
// block it.
func TestSettledEdgesInto_EachExclusionHasItsOwnWitness(t *testing.T) {
	node := func(id string) ir.Node { return &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}} }
	wf := &ir.Workflow{
		Name: "settled_provenance", Entry: "entry",
		Nodes: map[string]ir.Node{
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      node("a"), "b": node("b"), "unlaunched": node("unlaunched"),
			"live": node("live"), "taken": node("taken"), "rejected": node("rejected"),
			"behind_bounded": node("behind_bounded"), "looper": node("looper"),
			"after": node("after"), "past_the_join": node("past_the_join"),
			"join": node("join"),
		},
		Edges: []*ir.Edge{
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "router", To: "unlaunched"},
			{From: "router", To: "live"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
			// Witness 1 — provenance: reachable only from the branch this
			// invocation did NOT launch.
			{From: "unlaunched", To: "join"},
			// Witness 2 — routing: `live` ran and took `taken`; `rejected`
			// is reachable only through the edge routing turned down.
			{From: "live", To: "taken", Condition: "ok"},
			{From: "live", To: "rejected", IsElse: true},
			{From: "rejected", To: "join"},
			{From: "taken", To: "join"},
			// Witness 3 — bounded edges are not walked: `behind_bounded` sits
			// behind one, and its own edge into the join is ordinary.
			{From: "a", To: "behind_bounded", LoopName: "inner"},
			{From: "behind_bounded", To: "join"},
			// Witness 4 — the walk stops AT the join: `past_the_join` is
			// reachable only by continuing through it.
			{From: "join", To: "after"},
			{From: "after", To: "past_the_join"},
			{From: "past_the_join", To: "join"},
			// Witness 5 — a bounded edge INTO the join is never collected.
			// Its source has to be reachable by an ORDINARY edge, or the
			// stop-at-join guard excludes it first and the collect filter
			// is never asked.
			{From: "b", To: "looper"},
			{From: "looper", To: "join", LoopName: "retry"},
		},
	}
	// The seeds a `fan_out_all` produces: each launched edge's own target.
	launched := []*ir.Edge{wf.Edges[0], wf.Edges[1], wf.Edges[3]}
	seeds := settledSeedsPerEdge("router", launched, nil)
	// `live` ran and routed to `taken`, which ran too. Everything else in
	// this invocation produced nothing — which is what puts `a` and `b` in
	// the floor and keeps `taken` out.
	ev := invocationEvidence{
		ran: map[string]bool{"live": true, "taken": true},
		chosen: map[string][]store.IncomingEdge{
			"taken": {incomingFromEdge(wf.Edges[7])},
			"join":  {incomingFromEdge(wf.Edges[10])},
		},
	}
	got := settledEdgesInto(wf, seeds, "join", ev)

	froms := map[string]bool{}
	for _, in := range got {
		froms[in.From] = true
	}
	for _, tc := range []struct{ from, why string }{
		{"unlaunched", "provenance read the DECLARED edges instead of the launched ones"},
		{"rejected", "the walk crossed an edge routing turned down, so a node that never ran reached the join (#484)"},
		{"behind_bounded", "the walk crossed a bounded-iteration edge, which is a per-iteration overlay and not a stabilized forward pass (Rae4900)"},
		{"past_the_join", "the walk expanded PAST the join and came back into it from downstream"},
		{"looper", "a bounded-iteration edge into the join was collected"},
		{"after", "the walk expanded past the join through its own outgoing edge"},
		{"taken", "a source that RAN was collected; its edges are routing's recorded selection to apply"},
	} {
		if froms[tc.from] {
			t.Errorf("floor contains %s -> join: %s (floor = %+v)", tc.from, tc.why, got)
		}
	}
	if !froms["a"] || !froms["b"] {
		t.Fatalf("floor = %+v, want the launched branches' own edges into the join", got)
	}
}

// A resumed branch re-executes the node its durable cursor recorded, not the
// one the current graph's template edge names. An edited `fan_out_each`
// therefore runs the OLD start node while the declaration points at a new
// one, and provenance read from the declaration walks a branch this
// invocation is not running.
func TestSettledEdgesInto_RecordedStartNodesOutrankTheDeclaredTemplate(t *testing.T) {
	node := func(id string) ir.Node { return &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}} }
	wf := &ir.Workflow{
		Name: "settled_edited_template", Entry: "entry",
		Nodes: map[string]ir.Node{
			"dispatch":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "dispatch"}, RouterMode: ir.RouterFanOutEach},
			"old_handle": node("old_handle"), "new_handle": node("new_handle"),
			"collect": node("collect"),
		},
		Edges: []*ir.Edge{
			// The edit re-pointed the template; both collector edges remain.
			{From: "dispatch", To: "new_handle"},
			{From: "old_handle", To: "collect", With: []*ir.DataMapping{settledLiteral("route", "old")}},
			{From: "new_handle", To: "collect", With: []*ir.DataMapping{settledLiteral("route", "new")}},
		},
	}
	ev := invocationEvidence{ran: map[string]bool{}, chosen: map[string][]store.IncomingEdge{}}
	got := settledEdgesInto(wf, settledSeedsForTemplate(wf.Edges[0], []*branchResult{
		{branchID: "branch_dispatch_0", startNodeID: "old_handle"},
	}), "collect", ev)

	if len(got) != 1 || got[0].From != "old_handle" {
		t.Fatalf("floor = %+v, want old_handle -> collect — the cursor says the branch is running `old_handle`, so that is the branch whose edges settled", got)
	}
}

// A `fan_out_all` branch can bail BEFORE execBranch — a slot acquired on an
// already-cancelled fan-out, a resume-turn error, a panic caught before the
// start — and then reports no start node at all. Every branch there has its
// OWN target, so a sibling that did start says nothing about it: reading the
// invocation as a whole drops its entire subgraph from the floor.
func TestSettledSeedsPerEdge_ABranchThatNeverStartedKeepsItsOwnSeed(t *testing.T) {
	launched := []*ir.Edge{
		{From: "router", To: "a"},
		{From: "router", To: "b"},
		{From: "router", To: "resumed"},
	}
	seeds := settledSeedsPerEdge("router", launched, []*branchResult{
		{branchID: "branch_router_a", startNodeID: "a"},
		// Bailed before execBranch: a result, but no start node.
		{branchID: "branch_router_b"},
		// A durable cursor that re-anchored the branch elsewhere.
		{branchID: "branch_router_resumed", startNodeID: "elsewhere"},
	})

	want := map[string]bool{"a": true, "b": true, "elsewhere": true}
	got := map[string]bool{}
	for _, s := range seeds {
		got[s] = true
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("seeds = %v, missing %q — each launched edge answers for ITSELF: its branch's recorded start node, or its own target when that branch never started", seeds, id)
		}
	}
	if got["resumed"] {
		t.Fatalf("seeds = %v — a branch that DID report a start node must not also seed the declared target", seeds)
	}
}

// The floor belongs to the fan-out visit that settled it. A later visit
// arriving through a bypass edge is a different visit, and routing chose that
// path: feeding it the dead branches' mappings is the #484 shape through a
// door that skips the tracked/selected filter.
func TestSettledFloor_AForwardArrivalDropsAnEarlierVisitsFloor(t *testing.T) {
	wf := &ir.Workflow{
		Name: "settled_bypass", Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"join":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitBestEffort},
			"gate":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gate"}},
			"bypass": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "bypass"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "join", With: []*ir.DataMapping{settledRef("note", "entry", "note")}},
			{From: "b", To: "join", With: []*ir.DataMapping{settledRef("note", "entry", "note")}},
			{From: "join", To: "gate"},
			{From: "gate", To: "bypass", Condition: "again"},
			{From: "gate", To: "done", IsElse: true},
			// A FORWARD edge back into the collector, bypassing the fan-out.
			{From: "bypass", To: "join"},
		},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
	}

	var visits []map[string]any
	pass := 0
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) { return settledEntryOutput(), nil })
	exec.on("a", func(_ map[string]any) (map[string]any, error) { return nil, errors.New("fail A") })
	exec.on("b", func(_ map[string]any) (map[string]any, error) { return nil, errors.New("fail B") })
	exec.on("join", func(input map[string]any) (map[string]any, error) {
		visits = append(visits, input)
		return map[string]any{}, nil
	})
	exec.on("gate", func(_ map[string]any) (map[string]any, error) {
		pass++
		return map[string]any{"again": pass < 2}, nil
	})
	exec.on("bypass", func(_ map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })

	if err := New(wf, tmpStore(t), exec).Run(context.Background(), "run-559-bypass", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(visits) < 2 {
		t.Fatalf("the join ran %d time(s), want 2 — the bypass never fired", len(visits))
	}
	if visits[0]["note"] != "n" {
		t.Fatalf("first visit = %v — the fan-out's own visit must get its floor", visits[0])
	}
	if v, present := visits[1]["note"]; present {
		t.Fatalf("second visit note = %v — this visit arrived through the bypass edge routing selected, so the earlier fan-out's floor is not its input (#484)", v)
	}
}

// ---------------------------------------------------------------------------
// #484: an edge whose source RAN is routing's call, never the floor's
// ---------------------------------------------------------------------------

// The unselected `else` of a live source clears the source-has-output guard
// — only routing's recorded selection keeps it out. The floor must not
// reopen that door, which is why it admits output-less sources alone.
func TestSettledFloor_LiveSourceExclusiveSiblingsStayFiltered(t *testing.T) {
	got := runSettledFanOut(t, settledLiveGateWorkflow(), "run-559-live-gate", map[string]func() (map[string]any, error){
		"a":    func() (map[string]any, error) { return map[string]any{"done": true}, nil },
		"gate": func() (map[string]any, error) { return map[string]any{"ok": true}, nil },
		"b":    func() (map[string]any, error) { return nil, errors.New("fail B") },
	}, "finalize")

	if v, ok := got["escalate"]; ok {
		t.Fatalf("escalate = %v: the unselected else of a node that RAN reached the join through the floor (#484)", v)
	}
	if got["verdict"] != "from-when" {
		t.Fatalf("verdict = %v, want %q — routing chose the `when` edge", got["verdict"], "from-when")
	}
}

// The walk follows the graph, so it will happily cross an edge ROUTING
// TURNED DOWN and reach the join through a node that never executed. That
// node produced no output, so an eligibility rule reading output-presence
// admits it — #484 reopened through the back door.
func TestSettledFloor_NeverWalksThroughARejectedRoute(t *testing.T) {
	wf := &ir.Workflow{
		Name: "settled_rejected_route", Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":        &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"gate":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gate"}},
			"unchosen": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "unchosen"}},
			"b":        &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"join":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitBestEffort},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "gate"},
			{From: "gate", To: "join", Condition: "ok", With: []*ir.DataMapping{settledLiteral("verdict", "from-when")}},
			{From: "gate", To: "unchosen", IsElse: true},
			{From: "unchosen", To: "join", With: []*ir.DataMapping{settledLiteral("escalate", "yes")}},
			{From: "b", To: "join", With: []*ir.DataMapping{settledRef("note", "entry", "note")}},
			{From: "join", To: "done"},
		},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
	}

	got := runSettledFanOut(t, wf, "run-559-rejected-route", map[string]func() (map[string]any, error){
		"a":    func() (map[string]any, error) { return map[string]any{"done": true}, nil },
		"gate": func() (map[string]any, error) { return map[string]any{"ok": true}, nil },
		"unchosen": func() (map[string]any, error) {
			t.Error("`unchosen` must never execute — routing took the `when ok` edge")
			return nil, errors.New("unreachable")
		},
		"b": func() (map[string]any, error) { return nil, errors.New("fail B") },
	}, "join")

	if v, ok := got["escalate"]; ok {
		t.Fatalf("escalate = %v — the floor walked THROUGH the edge routing turned down and applied the mapping of a node that never ran (#484)", v)
	}
	if got["verdict"] != "from-when" || got["note"] != "n" {
		t.Fatalf("input = %v — pruning the rejected route must not cost the routes that WERE taken", got)
	}
}

// "Absent from the trunk's outputs" is not "never ran": processConvergence
// merges only the successful branches and never removes what an EARLIER
// invocation left for a branch that failed this time. Anchoring the floor on
// the trunk therefore rejects the very branch it exists to serve, while the
// ordinary pass rejects it too for not being in this visit's selection.
func TestSettledFloor_StaleOutputFromAnEarlierInvocationIsNotEvidenceItRan(t *testing.T) {
	wf := &ir.Workflow{
		Name: "settled_stale_output", Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"join":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitBestEffort},
			"gate":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gate"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "join"},
			{From: "b", To: "join", With: []*ir.DataMapping{settledRef("note", "entry", "note")}},
			{From: "join", To: "gate"},
			{From: "gate", To: "router", LoopName: "retry", Condition: "again"},
			{From: "gate", To: "done", IsElse: true},
		},
		Loops: map[string]*ir.Loop{
			"retry": {Name: "retry", MaxIterations: 3, Entries: map[string]bool{"router": true}, Body: map[string]bool{"router": true, "a": true, "b": true, "join": true, "gate": true}},
		},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Foreaches: map[string]*ir.Foreach{},
	}

	var visits []map[string]any
	pass := 0
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) { return settledEntryOutput(), nil })
	exec.on("a", func(_ map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		if pass == 0 {
			return map[string]any{"ok": true}, nil
		}
		return nil, errors.New("b fails on the second invocation")
	})
	exec.on("join", func(input map[string]any) (map[string]any, error) {
		visits = append(visits, input)
		return map[string]any{}, nil
	})
	exec.on("gate", func(_ map[string]any) (map[string]any, error) {
		pass++
		return map[string]any{"again": pass < 2}, nil
	})

	if err := New(wf, tmpStore(t), exec).Run(context.Background(), "run-559-stale", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(visits) < 2 {
		t.Fatalf("the join ran %d time(s), want 2 — the fan-out was not re-invoked", len(visits))
	}
	if visits[1]["note"] != "n" {
		t.Fatalf("second visit = %v — `b` failed THIS invocation, but its output from the previous one made it look alive, so its parent-sourced mapping was dropped by both the floor and the ordinary pass", visits[1])
	}
}

// The dead half of a partially-failed fan-out has the same claim as a
// wholly-failed one: its mapping reads a durable parent output the failure
// never touched.
func TestSettledFloor_PartialFailureKeepsTheDeadBranchesMapping(t *testing.T) {
	got := runSettledFanOut(t, settledLiveGateWorkflow(), "run-559-partial", map[string]func() (map[string]any, error){
		"a":    func() (map[string]any, error) { return map[string]any{"done": true}, nil },
		"gate": func() (map[string]any, error) { return map[string]any{"ok": true}, nil },
		"b":    func() (map[string]any, error) { return nil, errors.New("fail B") },
	}, "finalize")

	if got["note"] != "n" {
		t.Fatalf("note = %v, want %q — the failed branch's edge reads the PARENT's output, which its failure left untouched", got["note"], "n")
	}
}

// ---------------------------------------------------------------------------
// Two alternatives from one dead source: undecided, not decided by order
// ---------------------------------------------------------------------------

func TestSettledFloor_UndecidedAlternativesLeaveTheKeyUnset(t *testing.T) {
	wf := &ir.Workflow{
		Name: "settled_alternatives", Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":        &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":        &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"finalize": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "finalize"}, AwaitMode: ir.AwaitBestEffort},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "finalize", Condition: "ok", With: []*ir.DataMapping{
				settledLiteral("verdict", "from-when"),
				settledRef("shared", "entry", "note"),
			}},
			{From: "a", To: "finalize", IsElse: true, With: []*ir.DataMapping{
				settledLiteral("verdict", "from-else"),
				settledRef("shared", "entry", "note"),
			}},
			{From: "b", To: "finalize", With: []*ir.DataMapping{settledRef("expected_count", "entry", "count")}},
			{From: "finalize", To: "done"},
		},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
	}

	var logBuf bytes.Buffer
	got := runSettledFanOutOpts(t, wf, "run-559-undecided", map[string]func() (map[string]any, error){
		"a": func() (map[string]any, error) { return nil, errors.New("fail A") },
		"b": func() (map[string]any, error) { return nil, errors.New("fail B") },
	}, "finalize", WithLogger(log.New(log.LevelWarn, &logBuf)))

	if v, ok := got["verdict"]; ok {
		t.Fatalf("verdict = %v: two exclusive alternatives from a source that never ran disagree, and declaration order picked one — that is the guess #484 removed", v)
	}
	if got["shared"] != "n" {
		t.Fatalf("shared = %v, want %q — the alternatives AGREE on it, so it holds whichever would have fired", got["shared"], "n")
	}
	if got["expected_count"] != 2 {
		t.Fatalf("expected_count = %v, want 2 — a disagreement on one key must not drop the other edges", got["expected_count"])
	}
	if !bytes.Contains(logBuf.Bytes(), []byte(`disagree on "verdict"`)) {
		t.Fatalf("the dropped key was silent; log:\n%s", logBuf.String())
	}
}

// ---------------------------------------------------------------------------
// Lifetime: a join may be a loop head, whose selection the back-edge replaces
// ---------------------------------------------------------------------------

func TestSettledFloor_SurvivesLoopReentryAndYieldsToTheBackEdge(t *testing.T) {
	wf := &ir.Workflow{
		Name: "settled_loop_head", Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"join":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitBestEffort},
			"gate":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gate"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "join", With: []*ir.DataMapping{
				settledRef("expected_count", "entry", "count"),
				settledRef("note", "entry", "note"),
			}},
			{From: "b", To: "join", With: []*ir.DataMapping{
				settledRef("expected_count", "entry", "count"),
				settledRef("note", "entry", "note"),
			}},
			{From: "join", To: "gate"},
			{From: "gate", To: "join", LoopName: "retry", Condition: "again", With: []*ir.DataMapping{
				settledRef("expected_count", "gate", "bumped"),
			}},
			{From: "gate", To: "done", IsElse: true},
		},
		Loops: map[string]*ir.Loop{
			"retry": {Name: "retry", MaxIterations: 3, Entries: map[string]bool{"join": true}, Body: map[string]bool{"join": true, "gate": true}},
		},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Foreaches: map[string]*ir.Foreach{},
	}

	var visits []map[string]any
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) { return settledEntryOutput(), nil })
	exec.on("a", func(_ map[string]any) (map[string]any, error) { return nil, errors.New("fail A") })
	exec.on("b", func(_ map[string]any) (map[string]any, error) { return nil, errors.New("fail B") })
	exec.on("join", func(input map[string]any) (map[string]any, error) {
		visits = append(visits, input)
		return map[string]any{}, nil
	})
	exec.on("gate", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"again": len(visits) < 2, "bumped": 7}, nil
	})

	if err := New(wf, tmpStore(t), exec).Run(context.Background(), "run-559-loop", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(visits) < 2 {
		t.Fatalf("the join ran %d time(s), want 2 — the loop never re-entered", len(visits))
	}
	if visits[1]["note"] != "n" {
		t.Fatalf("second visit = %v — the floor vanished on re-entry: selectEdgeRS REPLACES the selection at a loop head, which is why the floor cannot live there (Rae4900)", visits[1])
	}
	if visits[1]["expected_count"] != 7 {
		t.Fatalf("expected_count = %v on re-entry, want 7 — the back-edge carries the value for the NEXT iteration and must outrank the floor", visits[1]["expected_count"])
	}
}

// ---------------------------------------------------------------------------
// It is a FLOOR, and it resolves in the edge's own namespace
// ---------------------------------------------------------------------------

// A live edge and a floor edge writing the same key: the floor is applied
// before both merge passes, so the live value wins. Moving the floor between
// the passes would leave every other assertion in this file green.
func TestSettledFloor_LiveEdgeOutranksTheFloorOnASharedKey(t *testing.T) {
	wf := settledSharedKeyWorkflow()
	got := runSettledFanOut(t, wf, "run-559-collision", map[string]func() (map[string]any, error){
		"a": func() (map[string]any, error) { return nil, errors.New("fail A") },
		"b": func() (map[string]any, error) { return map[string]any{"ok": true}, nil },
	}, "join")

	if got["shared"] != "from-live" {
		t.Fatalf("shared = %v, want %q — the floor outranked a live edge; it is a base under both passes, not one more edge in them", got["shared"], "from-live")
	}
}

// `{{input.x}}` on an edge is the SOURCE NODE'S output, and a source that
// never ran has none. Resolving a floor edge against the caller's scope would
// silently promote the run-level launch payload into that namespace — the
// coincidence #479 removed.
func TestSettledFloor_InputRefsDoNotFallBackToTheRunPayload(t *testing.T) {
	wf := settledSharedKeyWorkflow()
	for _, e := range wf.Edges {
		if e.From == "a" && e.To == "join" {
			e.With = append(e.With, &ir.DataMapping{
				Key:  "sneaked",
				Refs: []*ir.Ref{{Kind: ir.RefInput, Path: []string{"leak"}, Raw: "{{input.leak}}"}},
				Raw:  "{{input.leak}}",
			})
		}
	}
	wf.Vars["leak"] = &ir.Var{Name: "leak", Type: ir.VarString}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) { return settledEntryOutput(), nil })
	exec.on("a", func(_ map[string]any) (map[string]any, error) { return nil, errors.New("fail A") })
	exec.on("b", func(_ map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })
	var got map[string]any
	exec.on("join", func(input map[string]any) (map[string]any, error) {
		got = input
		return map[string]any{}, nil
	})
	if err := New(wf, tmpStore(t), exec).Run(context.Background(), "run-559-ns", map[string]any{"leak": "run-level payload"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	v, present := got["sneaked"]
	if !present {
		t.Fatalf("input = %v — the key must exist with an empty value, not vanish", got)
	}
	if v != nil {
		t.Fatalf("sneaked = %#v — {{input.leak}} on a floor edge read the RUN-LEVEL payload; that namespace is the source node's output, and this source produced none (#479)", v)
	}
}

// A reference that resolves to nil is still a VALID mapping: the field
// exists, its value is empty. Dropping the key is what leaves
// `{{input.<key>}}` unresolved in a downstream prompt.
func TestSettledFloor_KeepsTheKeyOfANilValuedMapping(t *testing.T) {
	wf := settledSharedKeyWorkflow()
	for _, e := range wf.Edges {
		if e.From == "a" && e.To == "join" {
			e.With = append(e.With, settledRef("dead_branch_review", "a", "review"))
		}
	}
	got := runSettledFanOut(t, wf, "run-559-nil-key", map[string]func() (map[string]any, error){
		"a": func() (map[string]any, error) { return nil, errors.New("fail A") },
		"b": func() (map[string]any, error) { return map[string]any{"ok": true}, nil },
	}, "join")

	v, present := got["dead_branch_review"]
	if !present {
		t.Fatalf("input = %v — a mapping reading the dead branch must keep its key with an empty value; dropping it is what surfaces template syntax to the model", got)
	}
	if v != nil {
		t.Fatalf("dead_branch_review = %#v, want nil — the branch produced nothing", v)
	}
}

// A nested fan-out settles its floor on the BRANCH runState, which is aliased
// onto the branch result and persisted on the branch cursor. A lazily created
// map would replace the alias instead of writing through it, and the floor
// would never reach the checkpoint.
func TestSettledFloor_BranchCursorCarriesANestedFloor(t *testing.T) {
	parent := &runState{
		outputs:           map[string]map[string]any{"entry": {"ok": true}},
		artifacts:         map[string]map[string]any{},
		artifactOwners:    map[string]string{},
		artifactRevisions: map[string]store.ArtifactRevisionRef{},
		artifactVersions:  map[string]int{},
		loopCounters:      map[string]int{},
	}
	result := initBranchResult(parent, "branch-x", nil)
	local := newBranchRunState(parent, nil, result)

	local.setSettledFloor("inner_join", []store.IncomingEdge{{From: "dead", To: "inner_join"}})
	if len(result.settledIncoming["inner_join"]) != 1 {
		t.Fatalf("branch result floor = %+v — setSettledFloor replaced the alias instead of writing through it", result.settledIncoming)
	}

	cp := branchCheckpointFromState(local, result, "inner_join", false)
	if len(cp.SettledIncoming["inner_join"]) != 1 {
		t.Fatalf("branch cursor = %+v — a nested floor does not survive a branch pause", cp.SettledIncoming)
	}
	revived := initBranchResult(parent, "branch-x", cp)
	if len(revived.settledIncoming["inner_join"]) != 1 || revived.settledIncoming["inner_join"][0].From != "dead" {
		t.Fatalf("revived branch floor = %+v — the cursor was written but never read back", revived.settledIncoming)
	}
}

// ---------------------------------------------------------------------------
// The artifact contract sees exactly what the resolver applies
// ---------------------------------------------------------------------------

// An edge the floor feeds into the join reaches it like any other, so a
// {{artifacts.*}} reference on it is a dependency of the join. The two
// guards used to be separate copies; one shared eligibility rule is what
// keeps a join from consuming an artifact its contract never named.
func TestSettledFloor_ArtifactDependencyEntersTheJoinsContract(t *testing.T) {
	settled := &ir.Edge{From: "dead", To: "join", With: []*ir.DataMapping{{
		Key: "reviewed_plan", Raw: "{{artifacts.plan}}",
		Refs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"plan"}}},
	}}}
	foreign := &ir.Edge{From: "outsider", To: "join", With: []*ir.DataMapping{{
		Key: "stray", Raw: "{{artifacts.notes}}",
		Refs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"notes"}}},
	}}}
	join := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "join"}, Publish: "report"}
	eng := &Engine{workflow: &ir.Workflow{Nodes: map[string]ir.Node{
		"planner":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "planner"}, Publish: "plan"},
		"noter":    &ir.ToolNode{BaseNode: ir.BaseNode{ID: "noter"}, Publish: "notes"},
		"dead":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "dead"}},
		"outsider": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "outsider"}},
		"join":     join,
	}, Edges: []*ir.Edge{settled, foreign}}}
	rs := &runState{
		outputs:          map[string]map[string]any{},
		artifacts:        map[string]map[string]any{"plan": {"ok": true}, "notes": {"ok": true}},
		artifactVersions: map[string]int{"planner": 1, "noter": 1},
		artifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan":  {NodeID: "planner", Version: 0},
			"notes": {NodeID: "noter", Version: 0},
		},
		settledIncoming: map[string][]store.IncomingEdge{"join": {incomingFromEdge(settled)}},
	}

	contract := eng.artifactContractFor("join", join, 0, rs)
	if len(contract.Dependencies) != 1 || contract.Dependencies[0].LogicalRef != "plan" {
		t.Fatalf("contract dependencies = %+v, want exactly the settled edge's artifact (%q); the foreign edge's %q must stay out", contract.Dependencies, "plan", "notes")
	}
}

// The node that would have chosen between two alternatives is not always
// their common source. A conditional head that never ran leaves BOTH of its
// mutually exclusive successors reachable and output-less, so they reach the
// join as two distinct sources — and a disagreement judged per source never
// sees them meet.
func TestSettledFloor_AlternativesFromTwoDeadSourcesAreStillUndecided(t *testing.T) {
	wf := &ir.Workflow{
		Name: "settled_cross_source", Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"head":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "head"}},
			"x":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "x"}},
			"y":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "y"}},
			"other":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "other"}},
			"join":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitBestEffort},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "head"},
			{From: "router", To: "other"},
			{From: "head", To: "x", Condition: "ok"},
			{From: "head", To: "y", IsElse: true},
			{From: "x", To: "join", With: []*ir.DataMapping{settledLiteral("verdict", "from-x")}},
			{From: "y", To: "join", With: []*ir.DataMapping{settledLiteral("verdict", "from-y")}},
			{From: "other", To: "join", With: []*ir.DataMapping{settledRef("note", "entry", "note")}},
			{From: "join", To: "done"},
		},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
	}

	var logBuf bytes.Buffer
	got := runSettledFanOutOpts(t, wf, "run-559-cross-source", map[string]func() (map[string]any, error){
		"head":  func() (map[string]any, error) { return nil, errors.New("head fails") },
		"other": func() (map[string]any, error) { return nil, errors.New("other fails") },
	}, "join", WithLogger(log.New(log.LevelWarn, &logBuf)))

	if v, ok := got["verdict"]; ok {
		t.Fatalf("verdict = %v — `x` and `y` are mutually exclusive and neither ran, so declaration order settled it; judging the disagreement per SOURCE never puts them in the same bucket", v)
	}
	if got["note"] != "n" {
		t.Fatalf("note = %v, want %q — refusing one disagreement must not cost the other sources' keys", got["note"], "n")
	}
	if !bytes.Contains(logBuf.Bytes(), []byte(`disagree on "verdict"`)) {
		t.Fatalf("the dropped key was silent; log:\n%s", logBuf.String())
	}
}

// A key the conflict rule refused to apply is not a dependency: the node
// never received it. Recording it anyway makes a later resume re-validate an
// artifact nobody consumed, and refuse over its absence.
func TestSettledFloor_ADiscardedKeyIsNotADependency(t *testing.T) {
	whenEdge := &ir.Edge{From: "dead", To: "join", Condition: "ok", With: []*ir.DataMapping{{
		Key: "choice", Raw: "{{artifacts.plan}}",
		Refs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"plan"}}},
	}}}
	elseEdge := &ir.Edge{From: "dead", To: "join", IsElse: true, With: []*ir.DataMapping{{
		Key: "choice", Raw: "{{artifacts.notes}}",
		Refs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"notes"}}},
	}}}
	join := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "join"}, Publish: "report"}
	eng := New(&ir.Workflow{Nodes: map[string]ir.Node{
		"planner": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "planner"}, Publish: "plan"},
		"noter":   &ir.ToolNode{BaseNode: ir.BaseNode{ID: "noter"}, Publish: "notes"},
		"dead":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "dead"}},
		"join":    join,
	}, Edges: []*ir.Edge{whenEdge, elseEdge}}, tmpStore(t), newStubExecutor())
	rs := &runState{
		outputs: map[string]map[string]any{},
		// Distinct bodies, or the two alternatives would genuinely AGREE and
		// the key would rightly be applied — the fixture has to make them
		// disagree for the discard to be what is under test.
		artifacts:        map[string]map[string]any{"plan": {"title": "ship"}, "notes": {"title": "park"}},
		artifactVersions: map[string]int{"planner": 1, "noter": 1},
		artifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan":  {NodeID: "planner", Version: 0},
			"notes": {NodeID: "noter", Version: 0},
		},
		settledIncoming: map[string][]store.IncomingEdge{
			"join": {incomingFromEdge(whenEdge), incomingFromEdge(elseEdge)},
		},
	}

	if got := eng.buildNodeInputRS("join", rs.scope()); len(got) != 0 {
		t.Fatalf("input = %v — the two alternatives disagree on `choice`, so it must be unset", got)
	}
	contract := eng.artifactContractFor("join", join, 0, rs)
	if len(contract.Dependencies) != 0 {
		t.Fatalf("contract dependencies = %+v — `choice` never reached the node's input, so neither artifact is a dependency; a required one is re-validated on resume and can refuse it", contract.Dependencies)
	}
}

// A human collector resumes through its own path, which rebuilds the contract
// state by hand. Handing it the selection alone publishes an approval whose
// contract omits the artifact the floor actually fed it.
func TestSettledFloor_HumanCollectorPublishesItsFloorDependency(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	if _, err := s.CreateRun(ctx, "human-floor", "wf", nil); err != nil {
		t.Fatal(err)
	}
	human := &ir.HumanNode{BaseNode: ir.BaseNode{ID: "approve"}, Publish: "approval"}
	// `dead` never ran: the branch that would have carried this edge failed,
	// which is exactly why the edge is on the floor rather than the selection.
	edge := &ir.Edge{From: "dead", To: "approve", With: []*ir.DataMapping{{
		Key: "plan", Raw: "{{artifacts.plan}}",
		Refs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"plan"}}},
	}}}
	eng := New(&ir.Workflow{Nodes: map[string]ir.Node{
		"planner": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "planner"}, Publish: "plan"},
		"dead":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "dead"}},
		"approve": human,
	}, Edges: []*ir.Edge{edge}}, s, newStubExecutor())

	if _, err := eng.materializeHumanArtifact(
		ctx, "human-floor", "approve", map[string]any{"approved": true},
		map[string]int{"planner": 1},
		map[string]map[string]any{"planner": {"ok": true}},
		map[string]map[string]any{"plan": {"ok": true}},
		map[string]store.ArtifactRevisionRef{"plan": {NodeID: "planner", Version: 0}},
		&store.Checkpoint{SettledIncoming: map[string][]store.IncomingEdge{"approve": {incomingFromEdge(edge)}}},
	); err != nil {
		t.Fatal(err)
	}
	artifact, err := s.LoadArtifact(ctx, "human-floor", "approve", 0)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Contract == nil || len(artifact.Contract.Dependencies) != 1 || artifact.Contract.Dependencies[0].NodeID != "planner" {
		t.Fatalf("human artifact contract = %+v — the collector consumed {{artifacts.plan}} through the floor, so its published approval depends on it", artifact.Contract)
	}
}

// The contract for a human collector is computed from a state built BY HAND
// on the resume path, and the floor's artifact refs now depend on how it
// RESOLVES. Hand that state fewer namespaces than the node's input was built
// from and the shared function answers the two callers differently: two
// alternatives that disagree through `{{vars.*}}` read nil == nil, agree, and
// record as REQUIRED an artifact the collector never consumed.
func TestSettledFloor_HumanContractResolvesInTheSameNamespacesAsTheInput(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	if _, err := s.CreateRun(ctx, "human-vars", "wf", nil); err != nil {
		t.Fatal(err)
	}
	human := &ir.HumanNode{BaseNode: ir.BaseNode{ID: "approve"}, Publish: "approval"}
	// The two alternatives differ ONLY through the var they read; the two
	// artifacts have identical bodies, so a path that cannot see vars finds
	// them equal and calls it agreement.
	alt := func(varName, artifactName string, isElse bool) *ir.Edge {
		raw := "{{vars." + varName + "}}/{{artifacts." + artifactName + "}}"
		return &ir.Edge{From: "dead", To: "approve", IsElse: isElse, Condition: map[bool]string{true: "", false: "ok"}[isElse], With: []*ir.DataMapping{{
			Key: "choice", Raw: raw,
			Refs: []*ir.Ref{
				{Kind: ir.RefVars, Path: []string{varName}},
				{Kind: ir.RefArtifacts, Path: []string{artifactName}},
			},
		}}}
	}
	whenEdge, elseEdge := alt("a_pick", "plan", false), alt("b_pick", "notes", true)
	eng := New(&ir.Workflow{Nodes: map[string]ir.Node{
		"planner": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "planner"}, Publish: "plan"},
		"noter":   &ir.ToolNode{BaseNode: ir.BaseNode{ID: "noter"}, Publish: "notes"},
		"dead":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "dead"}},
		"approve": human,
	}, Edges: []*ir.Edge{whenEdge, elseEdge}}, s, newStubExecutor())

	if _, err := eng.materializeHumanArtifact(
		ctx, "human-vars", "approve", map[string]any{"approved": true},
		map[string]int{"planner": 1, "noter": 1},
		map[string]map[string]any{},
		map[string]map[string]any{"plan": {"same": true}, "notes": {"same": true}},
		map[string]store.ArtifactRevisionRef{
			"plan":  {NodeID: "planner", Version: 0},
			"notes": {NodeID: "noter", Version: 0},
		},
		&store.Checkpoint{
			Vars: map[string]any{"a_pick": "A", "b_pick": "B"},
			SettledIncoming: map[string][]store.IncomingEdge{
				"approve": {incomingFromEdge(whenEdge), incomingFromEdge(elseEdge)},
			},
		},
	); err != nil {
		t.Fatal(err)
	}
	artifact, err := s.LoadArtifact(ctx, "human-vars", "approve", 0)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Contract != nil && len(artifact.Contract.Dependencies) != 0 {
		t.Fatalf("contract dependencies = %+v — the two alternatives disagree through {{vars.*}}, so `choice` never reached the collector; a contract blind to vars reads them as equal and records an artifact nobody consumed, which a later resume can refuse over", artifact.Contract.Dependencies)
	}
}

// Two alternatives carrying the SAME template agree by construction. Reading
// a moving namespace twice and comparing the two reads invents a
// disagreement, and the key is dropped from a node that should have had it.
func TestSettledFloor_IdenticalTemplatesDoNotDisagreeOverAMovingClock(t *testing.T) {
	elapsed := func(key string) *ir.DataMapping {
		return &ir.DataMapping{
			Key:  key,
			Refs: []*ir.Ref{{Kind: ir.RefRun, Path: []string{"elapsed_seconds"}, Raw: "{{run.elapsed_seconds}}"}},
			Raw:  "{{run.elapsed_seconds}}",
		}
	}
	wf := settledSharedKeyWorkflow()
	for _, e := range wf.Edges {
		if e.From == "a" && e.To == "join" {
			e.With = []*ir.DataMapping{elapsed("started_at"), settledLiteral("shared", "from-floor")}
		}
	}
	// The same source's `else` alternative carries the identical mapping.
	wf.Edges = append(wf.Edges, &ir.Edge{From: "a", To: "join", IsElse: true, With: []*ir.DataMapping{elapsed("started_at")}})

	got := runSettledFanOut(t, wf, "run-559-clock", map[string]func() (map[string]any, error){
		"a": func() (map[string]any, error) { return nil, errors.New("fail A") },
		"b": func() (map[string]any, error) { return map[string]any{"ok": true}, nil },
	}, "join")

	if _, present := got["started_at"]; !present {
		t.Fatalf("input = %v — both alternatives carry the IDENTICAL template, so they cannot disagree; only resolving it twice against a clock that moves makes them look like they do", got)
	}
}

// ---------------------------------------------------------------------------
// #1113: an empty fan_out_each, under either await mode
// ---------------------------------------------------------------------------

// The repro pins wait_all; the deletion the floor replaces is taken before
// the await mode is ever consulted, so best_effort has to answer the same.
func TestSettledFloor_EmptyFanOutEachUnderBestEffort(t *testing.T) {
	wf := fanOutEachWorkflow(false, ir.AwaitBestEffort, 0)
	settledAttachCollectMapping(wf)
	got := runSettledFanOutEach(t, wf, "run-1113-best-effort", map[string]any{"items": []any{}, "count": 0})
	if !settledSame(got["expected_count"], 0) {
		t.Fatalf("input = %v — an empty fan_out_each drops its parent-sourced mappings under best_effort too", got)
	}
}

// A DAG item whose dependency failed gets a SYNTHETIC branch result with no
// start node recorded, so a floor derived from what the branches reported
// would find nothing to walk from.
func TestSettledFloor_EmptyFanOutEachSkippedDagItem(t *testing.T) {
	wf := fanOutEachWorkflow(true, ir.AwaitBestEffort, 0)
	settledAttachCollectMapping(wf)
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B", "A")}, "count": 2}, nil
	})
	exec.on("handle", func(_ map[string]any) (map[string]any, error) { return nil, errors.New("A fails, B is skipped") })
	var got map[string]any
	exec.on("collect", func(input map[string]any) (map[string]any, error) {
		got = input
		return map[string]any{}, nil
	})
	if err := New(wf, tmpStore(t), exec).Run(context.Background(), "run-1113-dag", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got == nil {
		t.Fatal("the convergence node never ran")
	}
	if !settledSame(got["expected_count"], 2) {
		t.Fatalf("input = %v — a skipped DAG item carries no start node, so provenance cannot be read off the branch results", got)
	}
}

// ---------------------------------------------------------------------------
// Persistence: the failure that creates the floor is what parks the run
// ---------------------------------------------------------------------------

// The join itself failing is the ordinary way this run ends up parked, and
// the resume restarts AT the join without replaying the fan-out. A floor
// that lived only in memory would be gone exactly there.
func TestSettledFloor_SurvivesAResumeAtTheJoin(t *testing.T) {
	s := tmpStore(t)
	seen, _ := runSettledUntilParked(t, s, "run-559-resume", settledResumeWorkflow())

	r, err := s.LoadRun(context.Background(), "run-559-resume")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable", r.Status)
	}
	if len(r.Checkpoint.SettledIncoming["finalize"]) == 0 {
		t.Fatalf("the checkpoint carries no settled floor for the join: %+v", r.Checkpoint.SelectedIncoming)
	}

	// A NEW engine, so nothing rides in-process state across the resume.
	if err := New(settledResumeWorkflow(), s, seen.exec).Resume(context.Background(), "run-559-resume", nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(seen.inputs) != 2 {
		t.Fatalf("the join ran %d time(s), want 2", len(seen.inputs))
	}
	if !settledSame(seen.inputs[1]["from_a"], 2) || !settledSame(seen.inputs[1]["from_b"], "n") {
		t.Fatalf("input after resume = %v — the floor did not survive the checkpoint", seen.inputs[1])
	}
}

// `resume --force` re-executes against an edited .bot. The floor carries
// enough provenance to be revalidated EDGE BY EDGE: what still matches
// applies, what does not is dropped, and nothing is admitted that the
// invocation never settled. A per-node boolean would have let all three
// cases through.
func TestSettledFloor_RevalidatesPerEdgeAgainstAnEditedGraph(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*ir.Workflow)
		wantA any
		wantB any
	}{
		{
			name:  "one source renamed: its edge alone is dropped",
			edit:  func(wf *ir.Workflow) { renameSettledNode(wf, "agent_b", "agent_c") },
			wantA: 2, wantB: nil,
		},
		{
			name: "every source renamed: the floor empties, with no implicit fallback",
			edit: func(wf *ir.Workflow) {
				renameSettledNode(wf, "agent_a", "agent_x")
				renameSettledNode(wf, "agent_b", "agent_y")
			},
			wantA: nil, wantB: nil,
		},
		{
			name: "an edge added by the edit is not settled retroactively",
			edit: func(wf *ir.Workflow) {
				wf.Nodes["newcomer"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "newcomer"}}
				wf.Edges = append(wf.Edges, &ir.Edge{From: "newcomer", To: "finalize", With: []*ir.DataMapping{
					settledRef("from_new", "entry", "stale_count"),
				}})
			},
			wantA: 2, wantB: "n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			seen, _ := runSettledUntilParked(t, s, "run-559-force", settledResumeWorkflow())

			edited := settledResumeWorkflow()
			tc.edit(edited)
			if err := New(edited, s, seen.exec).Resume(context.Background(), "run-559-force", nil); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			got := seen.inputs[len(seen.inputs)-1]
			if !settledSame(got["from_a"], tc.wantA) {
				t.Fatalf("from_a = %v, want %v (input %v)", got["from_a"], tc.wantA, got)
			}
			if !settledSame(got["from_b"], tc.wantB) {
				t.Fatalf("from_b = %v, want %v (input %v)", got["from_b"], tc.wantB, got)
			}
			if _, present := got["from_new"]; present {
				t.Fatalf("input = %v: an edge the edit ADDED was applied as if the invocation had settled it — a floor revalidated per NODE admits every new edge into that node, which is why it has to be per EDGE", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// settledResumeWorkflow gives each dead branch a key of its own, so an
// edge-by-edge revalidation is observable one edge at a time.
func settledResumeWorkflow() *ir.Workflow {
	wf := fanOutWorkflow(ir.AwaitBestEffort)
	for _, e := range wf.Edges {
		switch {
		case e.From == "agent_a" && e.To == "finalize":
			e.With = append(e.With, settledRef("from_a", "entry", "count"))
		case e.From == "agent_b" && e.To == "finalize":
			e.With = append(e.With, settledRef("from_b", "entry", "note"))
		}
	}
	return wf
}

// renameSettledNode rewrites a node id and every edge that names it — the
// shape a `.bot` edit takes, and what makes a rehydrated floor identity
// match nothing on the current graph.
func renameSettledNode(wf *ir.Workflow, from, to string) {
	if node, ok := wf.Nodes[from]; ok {
		delete(wf.Nodes, from)
		wf.Nodes[to] = node
	}
	for _, e := range wf.Edges {
		if e.From == from {
			e.From = to
		}
		if e.To == from {
			e.To = to
		}
	}
}

// settledAttachCollectMapping puts a parent-sourced mapping on the
// fan_out_each template's edge into the convergence — the mapping that has
// nothing to do with the items and must survive an empty collection.
func settledAttachCollectMapping(wf *ir.Workflow) {
	for _, e := range wf.Edges {
		if e.From == "handle" && e.To == "collect" {
			e.With = []*ir.DataMapping{settledRef("expected_count", "entry", "count")}
		}
	}
}

func runSettledFanOutEach(t *testing.T, wf *ir.Workflow, runID string, entryOut map[string]any) map[string]any {
	t.Helper()
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) { return entryOut, nil })
	var got map[string]any
	exec.on("collect", func(input map[string]any) (map[string]any, error) {
		got = input
		return map[string]any{}, nil
	})
	if err := New(wf, tmpStore(t), exec).Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got == nil {
		t.Fatal("the convergence node never ran")
	}
	return got
}

type settledJoinProbe struct {
	inputs []map[string]any
	exec   *stubExecutor
}

// runSettledUntilParked runs wf until the join fails and the run parks
// failed_resumable, and hands back the probe so the caller can resume with
// a fresh engine.
func runSettledUntilParked(t *testing.T, s store.RunStore, runID string, wf *ir.Workflow) (*settledJoinProbe, error) {
	t.Helper()
	probe := &settledJoinProbe{exec: newStubExecutor()}
	probe.exec.on("entry", func(_ map[string]any) (map[string]any, error) { return settledEntryOutput(), nil })
	probe.exec.on("agent_a", func(_ map[string]any) (map[string]any, error) { return nil, errors.New("fail A") })
	probe.exec.on("agent_b", func(_ map[string]any) (map[string]any, error) { return nil, errors.New("fail B") })
	probe.exec.on("finalize", func(input map[string]any) (map[string]any, error) {
		probe.inputs = append(probe.inputs, input)
		if len(probe.inputs) == 1 {
			return nil, errors.New("transient")
		}
		return map[string]any{}, nil
	})
	err := New(wf, s, probe.exec).Run(context.Background(), runID, nil)
	if err == nil {
		t.Fatal("expected the join to fail the first pass")
	}
	return probe, err
}

// settledLongBranchWorkflow puts a second node between the launched edge and
// the edge that reaches the join, so a floor derived from the branch's start
// node alone comes up empty.
func settledLongBranchWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name: "settled_long_branch", Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"a2":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a2"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"b2":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b2"}},
			"join":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitBestEffort},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "a2"},
			{From: "b", To: "b2"},
			{From: "a2", To: "join", With: []*ir.DataMapping{settledRef("expected_count", "entry", "count")}},
			{From: "b2", To: "join", With: []*ir.DataMapping{settledRef("note", "entry", "note")}},
			{From: "join", To: "done"},
		},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
	}
}

// settledSharedKeyWorkflow has one dead branch and one live branch whose
// edges into the join write the SAME key, so precedence between the floor and
// the ordinary passes is observable.
func settledSharedKeyWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name: "settled_shared_key", Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"join":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitBestEffort},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			// Declared FIRST, so a floor applied in edge order rather than as
			// a base would still lose here; it is the PASS order that decides.
			{From: "a", To: "join", With: []*ir.DataMapping{settledLiteral("shared", "from-floor")}},
			{From: "b", To: "join", With: []*ir.DataMapping{settledLiteral("shared", "from-live")}},
			{From: "join", To: "done"},
		},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
	}
}

// settledLiveGateWorkflow has one branch that RUNS through a conditional
// gate and one that dies, so the same join sees both an edge routing chose
// between and an edge nobody could choose.
func settledLiveGateWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name: "settled_live_gate", Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":        &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"gate":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gate"}},
			"b":        &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"finalize": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "finalize"}, AwaitMode: ir.AwaitBestEffort},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "gate"},
			{From: "gate", To: "finalize", Condition: "ok", With: []*ir.DataMapping{settledLiteral("verdict", "from-when")}},
			// The unselected branch carries a key of its OWN beside the
			// shared one. Without it the disagreement rule would mask a
			// dropped has-output clause: two alternatives that collide on
			// every key leave nothing for the floor to leak.
			{From: "gate", To: "finalize", IsElse: true, With: []*ir.DataMapping{
				settledLiteral("verdict", "from-else"),
				settledLiteral("escalate", "yes"),
			}},
			{From: "b", To: "finalize", With: []*ir.DataMapping{settledRef("note", "entry", "note")}},
			{From: "finalize", To: "done"},
		},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
	}
}

func runSettledFanOut(t *testing.T, wf *ir.Workflow, runID string, behaviour map[string]func() (map[string]any, error), capture string) map[string]any {
	t.Helper()
	return runSettledFanOutOpts(t, wf, runID, behaviour, capture)
}

// runSettledFanOutOpts runs wf with a stub `entry` producing the shared
// parent output, the per-node behaviour given, and returns the input the
// capture node was handed.
func runSettledFanOutOpts(t *testing.T, wf *ir.Workflow, runID string, behaviour map[string]func() (map[string]any, error), capture string, opts ...EngineOption) map[string]any {
	t.Helper()
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) { return settledEntryOutput(), nil })
	for id, fn := range behaviour {
		exec.on(id, func(_ map[string]any) (map[string]any, error) { return fn() })
	}
	var got map[string]any
	exec.on(capture, func(input map[string]any) (map[string]any, error) {
		got = input
		return map[string]any{}, nil
	})
	if err := New(wf, tmpStore(t), exec, opts...).Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("run: %v (best_effort tolerates branch failures)", err)
	}
	if got == nil {
		t.Fatalf("the convergence node %q never ran", capture)
	}
	return got
}

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

// settledEdgesInto reads the edges the invocation LAUNCHED, never the ones
// the graph declares: an llm router in multi-select mode starts a subset,
// and the unlaunched sibling's downstream edge is a foreign edge.
func TestSettledEdgesInto_OnlyWalksTheLaunchedEdges(t *testing.T) {
	wf := &ir.Workflow{
		Name: "settled_provenance", Entry: "entry",
		Nodes: map[string]ir.Node{
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"c":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "c"}},
			"join":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}},
		},
		Edges: []*ir.Edge{
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "router", To: "c"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
			{From: "c", To: "join"},
			{From: "join", To: "a", LoopName: "retry"},
			{From: "join", To: "join", LoopName: "retry"},
		},
	}
	launched := []*ir.Edge{wf.Edges[0], wf.Edges[1]}
	got := settledEdgesInto(wf, launched, "join")

	froms := map[string]bool{}
	for _, in := range got {
		froms[in.From] = true
		if in.LoopName != "" || in.ForeachName != "" {
			t.Fatalf("a bounded-iteration edge entered the floor: %+v — a back-edge is a per-iteration overlay, not a stabilized forward pass", in)
		}
	}
	if !froms["a"] || !froms["b"] {
		t.Fatalf("floor = %+v, want the launched branches' edges into the join", got)
	}
	if froms["c"] {
		t.Fatalf("floor = %+v: the unlaunched branch's edge was settled — provenance read the DECLARED edges, not the launched ones", got)
	}
	if froms["join"] {
		t.Fatalf("floor = %+v: the walk expanded past the join and came back through the cycle", got)
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

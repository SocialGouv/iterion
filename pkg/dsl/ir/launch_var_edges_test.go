package ir

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/treeskip"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// whenEdge is a `from -> to when <src>` edge, carrying the parsed AST the
// compiler attaches to the clause.
func whenEdge(from, to, src string) *Edge {
	return &Edge{From: from, To: to, Expression: expr.MustParse(src), ExpressionSrc: src}
}

func varDecl(name string, vt VarType, def any) *Var {
	v := &Var{Name: name, Type: vt}
	if def != nil {
		v.HasDefault, v.Default = true, def
	}
	return v
}

// pickGraph is one node choosing between two targets: the shape every edge
// form is read in here. mode is the source router's selection.
func pickGraph(mode RouterMode, vars map[string]*Var, edges ...*Edge) *Workflow {
	return &Workflow{
		Name: "pick", Entry: "pick", Vars: vars,
		Nodes: map[string]Node{
			"pick": &RouterNode{BaseNode: BaseNode{ID: "pick"}, RouterMode: mode},
			"a":    &AgentNode{BaseNode: BaseNode{ID: "a"}},
			"b":    &AgentNode{BaseNode: BaseNode{ID: "b"}},
			"done": &DoneNode{BaseNode: BaseNode{ID: "done"}},
		},
		Edges: append(edges, &Edge{From: "a", To: "done"}, &Edge{From: "b", To: "done"}),
	}
}

// iterationGraph is a review loop: the judge's ITERATION back-edge (a `loop` or
// a `foreach` clause, carried by back) beside the exit the run takes once the
// iteration is spent.
func iterationGraph(vars map[string]*Var, back, exit *Edge) *Workflow {
	return &Workflow{
		Name: "review", Entry: "work", Vars: vars,
		Nodes: map[string]Node{
			"work":  &AgentNode{BaseNode: BaseNode{ID: "work"}},
			"judge": &JudgeNode{BaseNode: BaseNode{ID: "judge"}},
			"done":  &DoneNode{BaseNode: BaseNode{ID: "done"}},
		},
		Edges:     []*Edge{{From: "work", To: "judge"}, back, exit},
		Loops:     map[string]*Loop{"review": {Name: "review", MaxIterations: 3}},
		Foreaches: map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}},
	}
}

// cycleGraph is a router whose every branch is guarded by the same var, one of
// them into a node that comes back: under `f == "loop"` the surviving
// transitions form a cycle no terminal leaves, while every node keeps one.
func cycleGraph(vars map[string]*Var) *Workflow {
	return &Workflow{
		Name: "cycle", Entry: "pick", Vars: vars,
		Nodes: map[string]Node{
			"pick": &RouterNode{BaseNode: BaseNode{ID: "pick"}, RouterMode: RouterCondition},
			"a":    &AgentNode{BaseNode: BaseNode{ID: "a"}},
			"done": &DoneNode{BaseNode: BaseNode{ID: "done"}},
		},
		Edges: []*Edge{
			whenEdge("pick", "a", "vars.f == 'loop'"),
			whenEdge("pick", "done", "vars.f == 'exit'"),
			{From: "a", To: "pick"},
		},
	}
}

// iterationEdge is a back-edge carrying an iteration clause and a `when`.
func iterationEdge(clause, src string) *Edge {
	e := whenEdge("judge", "work", src)
	if clause == "loop" {
		e.LoopName = "review"
		return e
	}
	e.ForeachName = "items"
	return e
}

// droppedEdges names the targets a filter refuses, in the graph's edge order.
// A nil filter refuses nothing — the unfiltered walk.
func droppedEdges(w *Workflow, open EdgeFilter) []string {
	var out []string
	if open == nil {
		return nil
	}
	for _, e := range w.Edges {
		if !open(e) {
			out = append(out, e.To)
		}
	}
	return out
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The filter drops a transition only where it reads the run's launch vars the
// way the engine will — and the fallback a matching guard pre-empts, which is
// the transition a pre-flight was counting under a launch that had already
// chosen (#1828). Everything else keeps the whole-graph answer.
func TestLaunchVarEdgeFilterDropsOnlyWhatTheLaunchDecided(t *testing.T) {
	family := func(def any) map[string]*Var {
		return map[string]*Var{"f": varDecl("f", VarString, def)}
	}
	cfg := func(def any) map[string]*Var {
		return map[string]*Var{"cfg": varDecl("cfg", VarJSON, def)}
	}
	for _, tc := range []struct {
		name   string
		wf     *Workflow
		launch map[string]any
		want   []string // the targets the filter drops
	}{
		{
			name: "a guard the launch made false",
			wf: pickGraph(RouterCondition, family("claude"),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				whenEdge("pick", "b", "vars.f == 'claude'")),
			want: []string{"a"},
		},
		{
			name: "the bare fallback a matching guard pre-empts",
			wf: pickGraph(RouterCondition, family("claude"),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				&Edge{From: "pick", To: "b"}),
			launch: map[string]any{"f": "gpt"},
			want:   []string{"b"},
		},
		{
			name: "the else fallback a matching guard pre-empts",
			wf: pickGraph(RouterCondition, family("gpt"),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				&Edge{From: "pick", To: "b", IsElse: true}),
			want: []string{"b"},
		},
		{
			name: "a fallback with no matching guard stands",
			wf: pickGraph(RouterCondition, family("claude"),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				&Edge{From: "pick", To: "b"}),
			want: []string{"a"},
		},
		{
			name: "an undecided sibling of a matching guard stands: it may win first",
			wf: pickGraph(RouterCondition, family("gpt"),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				&Edge{From: "pick", To: "b", Condition: "ready"}),
			want: nil,
		},
		{
			name: "the fallback is pre-empted whatever else is undecided",
			wf: pickGraph(RouterCondition, family("gpt"),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				&Edge{From: "pick", To: "done", Condition: "ready"},
				&Edge{From: "pick", To: "b"}),
			want: []string{"b"},
		},
		{
			name: "a var no default and no launch value answers",
			wf: pickGraph(RouterCondition, family(nil),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			name: "a launch value for a var the workflow does not declare",
			wf: pickGraph(RouterCondition, map[string]*Var{"other": varDecl("other", VarString, "x")},
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				&Edge{From: "pick", To: "b"}),
			launch: map[string]any{"f": "claude"},
			want:   nil,
		},
		{
			name: "a predicate reading a node output beside the var",
			wf: pickGraph(RouterCondition, family("claude"),
				whenEdge("pick", "a", "vars.f == 'gpt' && outputs.pick.ready"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			// The namespace is what decides, never the name: `input.f` is a
			// field of the source node's output that happens to be spelled
			// like a var, and the run answers it from somewhere else entirely.
			name: "a predicate reading the input namespace at a name a var carries too",
			wf: pickGraph(RouterCondition, family("claude"),
				whenEdge("pick", "a", "input.f == 'gpt'"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			name: "a predicate reading a bare identifier (the implicit input)",
			wf: pickGraph(RouterCondition, family("claude"),
				whenEdge("pick", "a", "approved"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			name: "a predicate reading a loop counter",
			wf: pickGraph(RouterCondition, family("claude"),
				whenEdge("pick", "a", "loop.review.iteration > 1"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			// Dropping a node's LAST transition would have the walk judge a
			// run that cannot leave it: the engine dies there of a missing
			// edge, and `AlwaysReachesVia` — which reads "no path reaches a
			// terminal" as "every path is walled" — would let a hard cap take
			// the blame. Both walks keep the whole-graph answer instead,
			// routes behind that node included.
			name: "a node whose every transition the launch excluded",
			wf: pickGraph(RouterCondition, family("claude"),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				whenEdge("pick", "b", "vars.f == 'other'")),
			want: nil,
		},
		{
			name: "a predicate reading an artifact",
			wf: pickGraph(RouterCondition, family("claude"),
				whenEdge("pick", "a", "artifacts.plan.ready"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			// A vars-only predicate the evaluator REFUSES is not an answer:
			// the engine skips an edge whose `when` failed, and a fallback
			// beside it may then be the crossing.
			name: "a vars-only predicate the evaluator cannot answer",
			wf: pickGraph(RouterCondition, family("claude"),
				whenEdge("pick", "a", "vars.f > 3"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			name: "a predicate referencing nothing at all",
			wf: pickGraph(RouterCondition, family("claude"),
				whenEdge("pick", "a", "1 == 2"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			name: "the Condition field form, which reads the source's output",
			wf: pickGraph(RouterCondition, family("claude"),
				&Edge{From: "pick", To: "a", Condition: "use_gpt"},
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			name: "a fan-out branch, which runs whatever its predicate says",
			wf: pickGraph(RouterFanOutAll, family("claude"),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				whenEdge("pick", "b", "vars.f == 'claude'")),
			want: nil,
		},
		{
			name: "a data-driven fan-out branch",
			wf: pickGraph(RouterFanOutEach, family("claude"),
				whenEdge("pick", "a", "vars.f == 'gpt'")),
			want: nil,
		},
		{
			name: "an llm router's candidate, which the model may pick anyway",
			wf: pickGraph(RouterLLM, family("claude"),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			name: "a round-robin router's guarded edge, which it never takes",
			wf: pickGraph(RouterRoundRobin, family("claude"),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				&Edge{From: "pick", To: "b"},
				&Edge{From: "pick", To: "done"}),
			want: []string{"a"},
		},
		{
			// Round-robin cycles its GUARDLESS edges and reads no `when`
			// (runtime.execRoundRobin): each is taken in turn whatever a
			// guarded sibling decides, so no guard pre-empts one.
			name: "a round-robin router's cycled edges, which no guard pre-empts",
			wf: pickGraph(RouterRoundRobin, family("gpt"),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				&Edge{From: "pick", To: "b"},
				&Edge{From: "pick", To: "done"}),
			want: nil,
		},
		{
			// An iteration back-edge whose `when` holds is still declined at
			// its cap, its fuel, a liveness stall or its budget
			// (runtime.evaluateEdgesWithLoopsRS) — and the exit beside it is
			// what the run takes then.
			name: "the exit a guarded loop back-edge does not pre-empt",
			wf: iterationGraph(family("gpt"),
				iterationEdge("loop", "vars.f == 'gpt'"),
				&Edge{From: "judge", To: "done"}),
			want: nil,
		},
		{
			name: "the exit a guarded foreach back-edge does not pre-empt",
			wf: iterationGraph(family("gpt"),
				iterationEdge("foreach", "vars.f == 'gpt'"),
				&Edge{From: "judge", To: "done"}),
			want: nil,
		},
		{
			// The other direction holds: a back-edge whose `when` the launch
			// made false is never a candidate — the engine reads the clause
			// only once the condition does.
			name: "a loop back-edge the launch made false",
			wf: iterationGraph(family("claude"),
				iterationEdge("loop", "vars.f == 'gpt'"),
				&Edge{From: "judge", To: "done"}),
			want: []string{"work"},
		},
		{
			// resolveVars REWRITES a var whose value is the repo root, to the
			// worktree or in-container workspace the run works in (its
			// foot-gun guard) — a path no pre-flight holds.
			name: "a var holding an absolute path, which the run may read remapped",
			wf: pickGraph(RouterCondition, family("/srv/repo"),
				whenEdge("pick", "a", "vars.f == '/srv/repo'"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			name: "a json member that is an absolute path",
			wf: pickGraph(RouterCondition, cfg(`{"family":"claude","dir":"/srv/repo"}`),
				whenEdge("pick", "a", "vars.cfg.dir == '/srv/repo'"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			// The refusal follows what the REFERENCE reads: a path in a
			// sibling member is remapped on its own, and decides nothing here.
			name: "a json member beside an absolute-path sibling",
			wf: pickGraph(RouterCondition, cfg(`{"family":"claude","dir":"/srv/repo"}`),
				whenEdge("pick", "a", "vars.cfg.family == 'gpt'"),
				&Edge{From: "pick", To: "b"}),
			want: []string{"a"},
		},
		{
			name: "a member of a json var the view carries",
			wf: pickGraph(RouterCondition, cfg(`{"family":"claude"}`),
				whenEdge("pick", "a", "vars.cfg.family == 'gpt'"),
				&Edge{From: "pick", To: "b"}),
			want: []string{"a"},
		},
		{
			name: "a member of a json var the view does not carry",
			wf: pickGraph(RouterCondition, cfg(`{"family":"claude"}`),
				whenEdge("pick", "a", "vars.cfg.absent == 'gpt'"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			// The reference reads the LIST, not a leaf of it: the path lives
			// in an element, and remapRepoRoot rewrites elements.
			name: "a list with an absolute path in an element",
			wf: pickGraph(RouterCondition, map[string]*Var{"paths": varDecl("paths", VarStringArray, "/srv/repo,rel/x")},
				whenEdge("pick", "a", "contains(vars.paths, '/srv/repo')"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			// And the DOCUMENT, not a member: remapRepoRoot rewrites members.
			name: "a document with an absolute path in a member",
			wf: pickGraph(RouterCondition, cfg(`{"dir":"/srv/repo"}`),
				whenEdge("pick", "a", "length(keys(vars.cfg)) == 2"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			// Control for both: the same references on values with no path in
			// them DO decide, so the refusal is the path's and not the shape's.
			name: "a list with no absolute path in it",
			wf: pickGraph(RouterCondition, map[string]*Var{"paths": varDecl("paths", VarStringArray, "rel/x,rel/y")},
				whenEdge("pick", "a", "contains(vars.paths, '/srv/repo')"),
				&Edge{From: "pick", To: "b"}),
			want: []string{"a"},
		},
		{
			name: "a document with no absolute path in it",
			wf: pickGraph(RouterCondition, cfg(`{"dir":"rel/x"}`),
				whenEdge("pick", "a", "length(keys(vars.cfg)) == 2"),
				&Edge{From: "pick", To: "b"}),
			want: []string{"a"},
		},
		{
			// A var whose TEXT the environment answers is read again when the
			// engine resolves the run's vars — after the clone, the sandbox
			// start, or a capped window's reopening — and the settings overlay
			// is repointable while a run waits. Two readings, one premise: the
			// value must be the same text both times.
			name: "a var whose default reads the environment",
			wf: pickGraph(RouterCondition, family("${ITERION_ZZ_FAMILY:-claude}"),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				&Edge{From: "pick", To: "b"}),
			want: nil,
		},
		{
			name: "a launch value that reads the environment",
			wf: pickGraph(RouterCondition, family("claude"),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				&Edge{From: "pick", To: "b"}),
			launch: map[string]any{"f": "${ITERION_ZZ_FAMILY:-gpt}"},
			want:   nil,
		},
		{
			// The launch value is the text the run reads: an env-reading
			// DEFAULT a plain launch value overrode decides again.
			name: "a plain launch value over an env-reading default",
			wf: pickGraph(RouterCondition, family("${ITERION_ZZ_FAMILY:-claude}"),
				whenEdge("pick", "a", "vars.f == 'gpt'"),
				&Edge{From: "pick", To: "b"}),
			launch: map[string]any{"f": "claude"},
			want:   []string{"a"},
		},
		{
			// A prune that leaves the entry unable to reach a terminal is no
			// prune at all: AlwaysReachesVia would then answer "every path is
			// walled" for any wall, including one no execution crosses. Each
			// node here keeps a transition, so the per-node refusal does not
			// fire — the graph's does.
			name: "a prune that leaves the entry no terminal to reach",
			wf:   cycleGraph(family("loop")),
			want: nil,
		},
		{
			// Control: the same cycle under a launch that keeps the exit.
			name: "a prune that leaves a terminal reachable",
			wf:   cycleGraph(family("exit")),
			want: []string{"a"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := droppedEdges(tc.wf, tc.wf.LaunchVarEdgeFilter(tc.launch))
			if !sameStrings(got, tc.want) {
				t.Errorf("dropped %v, want %v", got, tc.want)
			}
		})
	}

	// A run with nothing to fold gets NO filter, not an empty one: the walk is
	// then the unfiltered walk, down to the same code path.
	for name, wf := range map[string]*Workflow{
		"a workflow declaring no var": pickGraph(RouterCondition, nil,
			whenEdge("pick", "a", "vars.f == 'gpt'"), &Edge{From: "pick", To: "b"}),
		"a declared var nothing answers": pickGraph(RouterCondition, family(nil),
			whenEdge("pick", "a", "vars.f == 'gpt'"), &Edge{From: "pick", To: "b"}),
		"a graph the launch decides nothing in": pickGraph(RouterCondition, family("claude"),
			&Edge{From: "pick", To: "a", Condition: "ready"}, &Edge{From: "pick", To: "b"}),
	} {
		if wf.LaunchVarEdgeFilter(nil) != nil {
			t.Errorf("%s: want no filter at all", name)
		}
	}
	if (*Workflow)(nil).LaunchVarEdgeFilter(map[string]any{"f": "gpt"}) != nil {
		t.Error("a nil workflow: want no filter at all")
	}
}

// escapeGraph is the shape the refusals below DECIDE in: a condition router
// whose guarded branch the launch answers, an undecided sibling into an
// `escape` that reaches a terminal on its own, and the bare fallback they
// pre-empt. Whatever the suspect node loses, the entry still reaches `done`
// through the escape, so the GRAPH-level refusal — which drops the whole
// filter when no path reaches a terminal — stays silent and cannot answer in
// another refusal's place.
func escapeGraph(vars map[string]*Var, suspect ...*Edge) *Workflow {
	edges := []*Edge{
		whenEdge("pick", "escape", "outputs.pick.hot"),
		whenEdge("pick", "work", "vars.f == 'gpt'"),
		{From: "pick", To: "stale"},
	}
	edges = append(edges, suspect...)
	edges = append(edges,
		&Edge{From: "work", To: "judge"},
		&Edge{From: "escape", To: "done"},
		&Edge{From: "stale", To: "done"},
		&Edge{From: "capped", To: "done"},
		&Edge{From: "x", To: "done"},
		&Edge{From: "y", To: "done"},
	)
	return &Workflow{
		Name: "escape", Entry: "pick", Vars: vars,
		Nodes: map[string]Node{
			"pick":   &RouterNode{BaseNode: BaseNode{ID: "pick"}, RouterMode: RouterCondition},
			"escape": &AgentNode{BaseNode: BaseNode{ID: "escape"}},
			"stale":  &AgentNode{BaseNode: BaseNode{ID: "stale"}},
			"work":   &AgentNode{BaseNode: BaseNode{ID: "work"}},
			"judge":  &JudgeNode{BaseNode: BaseNode{ID: "judge"}},
			"capped": &AgentNode{BaseNode: BaseNode{ID: "capped"}},
			"x":      &AgentNode{BaseNode: BaseNode{ID: "x"}},
			"y":      &AgentNode{BaseNode: BaseNode{ID: "y"}},
			"done":   &DoneNode{BaseNode: BaseNode{ID: "done"}},
		},
		Edges:     edges,
		Loops:     map[string]*Loop{"review": {Name: "review", MaxIterations: 3}},
		Foreaches: map[string]*Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}},
	}
}

// Each refusal is measured where the other one cannot answer for it. A
// suspect node on the entry's ONLY way out — pickGraph, iterationGraph — puts
// the graph-level refusal first: strip that node's transitions and no path
// reaches a terminal, so there is no filter at all and a case built there
// holds whatever the per-node refusal and the iteration carve-out do. With an
// escape beside it, the filter stands and what the suspect node keeps is what
// is measured.
func TestLaunchVarEdgeFilterRefusalsAreIsolated(t *testing.T) {
	family := map[string]*Var{"f": varDecl("f", VarString, "claude")}
	gpt := map[string]any{"f": "gpt"}
	for _, tc := range []struct {
		name    string
		suspect []*Edge
		want    []string
	}{
		{
			// A `foreach` back-edge whose `when` holds is still skipped once
			// the collection is spent (runtime.evaluateEdgesWithLoopsRS reads
			// `idx+1 >= count` and falls through before the predicate can be
			// the crossing), and the exit beside it is the crossing then: it
			// pre-empts no fallback of its node.
			name:    "a guarded foreach back-edge pre-empts no exit of its node",
			suspect: []*Edge{iterationEdge("foreach", "vars.f == 'gpt'"), {From: "judge", To: "capped"}},
			want:    []string{"stale"},
		},
		{
			// The loop half of that carve-out, measured the same way.
			name:    "a guarded loop back-edge pre-empts no exit of its node",
			suspect: []*Edge{iterationEdge("loop", "vars.f == 'gpt'"), {From: "judge", To: "capped"}},
			want:    []string{"stale"},
		},
		{
			// A node whose EVERY transition the launch excluded keeps them
			// all: the engine dies there of its own missing edge, and a walk
			// reading that node as a wall would let a hard cap take the blame
			// for that death. The transitions kept keep the routes behind it
			// on the walk too, which is the half this buys nothing for.
			name: "a node whose every transition the launch excluded keeps them",
			suspect: []*Edge{
				whenEdge("judge", "x", "vars.f == 'claude'"),
				whenEdge("judge", "y", "vars.f == 'other'"),
			},
			want: []string{"stale"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := escapeGraph(family, tc.suspect...)
			open := wf.LaunchVarEdgeFilter(gpt)
			// The premise of all three: the fallback a holding guard
			// pre-empts is dropped and the entry still reaches a terminal
			// through the escape. Without it this case measures nothing
			// about the refusal it names.
			if open == nil {
				t.Fatal("no filter at all: the graph-level refusal answered, so this case decides nothing about the refusal it names")
			}
			if got := droppedEdges(wf, open); !sameStrings(got, tc.want) {
				t.Errorf("dropped %v, want %v", got, tc.want)
			}
		})
	}
}

// The walks read the run's own topology through the filter: a target only an
// excluded branch led to is unreachable, and a wall the remaining branches all
// cross is unavoidable. A graph the walk refuses stays refused whatever the
// filter says — a dangling edge is not a predicate's to drop.
func TestReachWalksReadTheOpenEdgesOnly(t *testing.T) {
	// review-pr's mono topology: the gpt branch guarded, the claude branch the
	// bare fallback (bots/review-pr/main.bot).
	topology := func() *Workflow {
		return &Workflow{
			Name: "review_pr", Entry: "topology",
			Vars: map[string]*Var{"mono_family": varDecl("mono_family", VarString, "claude")},
			Nodes: map[string]Node{
				"topology":        &RouterNode{BaseNode: BaseNode{ID: "topology"}, RouterMode: RouterCondition},
				"reviewer_claude": &AgentNode{BaseNode: BaseNode{ID: "reviewer_claude"}},
				"reviewer_gpt":    &AgentNode{BaseNode: BaseNode{ID: "reviewer_gpt"}},
				"done":            &DoneNode{BaseNode: BaseNode{ID: "done"}},
			},
			Edges: []*Edge{
				whenEdge("topology", "reviewer_gpt", "vars.mono_family == 'gpt'"),
				{From: "topology", To: "reviewer_claude"},
				{From: "reviewer_gpt", To: "done"},
				{From: "reviewer_claude", To: "done"},
			},
		}
	}
	isClaude := func(n Node) bool { return n != nil && n.NodeID() == "reviewer_claude" }
	gpt := map[string]any{"mono_family": "gpt"}

	wf := topology()
	if !wf.CanReach(isClaude) {
		t.Fatal("precondition: the unfiltered walk reaches the claude reviewer")
	}
	if wf.CanReachVia(wf.LaunchVarEdgeFilter(gpt), isClaude) {
		t.Error("CanReachVia: a mono-gpt launch cannot reach the claude reviewer — its edge is the fallback a matching guard pre-empts")
	}
	if !wf.CanReachVia(wf.LaunchVarEdgeFilter(map[string]any{"mono_family": "claude"}), isClaude) {
		t.Error("CanReachVia: a mono-claude launch takes the fallback to the claude reviewer")
	}
	if wf.AlwaysReaches(isClaude) {
		t.Fatal("precondition: the unfiltered walk finds a path around the claude reviewer")
	}
	if !wf.AlwaysReachesVia(wf.LaunchVarEdgeFilter(map[string]any{"mono_family": "claude"}), isClaude) {
		t.Error("AlwaysReachesVia: a mono-claude launch excludes the gpt branch — no path avoids the claude reviewer")
	}
	if wf.AlwaysReachesVia(wf.LaunchVarEdgeFilter(gpt), isClaude) {
		t.Error("AlwaysReachesVia: a mono-gpt launch runs the gpt branch, which avoids the claude reviewer")
	}

	// A dangling edge the entry reaches keeps both walks conservative, even
	// when the launch vars decided its predicate false.
	dangling := topology()
	dangling.Edges = append(dangling.Edges, whenEdge("topology", "ghost", "vars.mono_family == 'claude'"))
	open := dangling.LaunchVarEdgeFilter(gpt)
	if !dangling.CanReachVia(open, isClaude) {
		t.Error("CanReachVia: an edge into an undefined node must keep the walk conservative")
	}
	if !dangling.AlwaysReachesVia(open, isClaude) {
		t.Error("AlwaysReachesVia: an edge into an undefined node must keep the walk conservative")
	}
	// The malformed edge BEHIND an excluded branch is the other half of what
	// the carve-out does and does not guarantee: the edge is never dropped
	// from its source's successor list, but a filter that puts the SOURCE off
	// the run's paths does conclude — on the run, which never reaches it. The
	// case above puts the dangling edge on the entry, which no filter can
	// make unreachable, so only this one exercises the difference.
	behind := topology()
	behind.Edges = append(behind.Edges, &Edge{From: "reviewer_claude", To: "ghost"})
	behindOpen := behind.LaunchVarEdgeFilter(gpt)
	if behind.CanReachVia(behindOpen, isClaude) {
		t.Error("CanReachVia: a mono-gpt run reaches neither the claude reviewer nor the malformed edge behind it")
	}
	if behind.AlwaysReachesVia(behindOpen, isClaude) {
		t.Error("AlwaysReachesVia: a mono-gpt run takes the gpt branch, which is walled by nothing")
	}
	if !behind.CanReach(isClaude) || !behind.AlwaysReaches(isClaude) {
		t.Error("the unfiltered walks must still refuse the malformed graph")
	}
}

// isRunVars reports whether an expression names a `vars` field — the run
// state's own map, or the resolution scope built from it, which a reading of
// shapes cannot tell apart and which hold the same map anyway.
func isRunVars(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "vars"
}

// buildsARunState reports whether a composite literal builds a run state. A
// literal whose type is ELIDED is judged as one: no reading here can narrow
// it, so it falls on the side that judges.
func buildsARunState(typ ast.Expr) bool {
	switch t := typ.(type) {
	case nil:
		return true
	case *ast.Ident:
		return t.Name == "runState"
	}
	return false
}

// varsTypeName is how a composite literal's type is spelled, for the message
// and for the list of the other fields named `vars`.
func varsTypeName(typ ast.Expr) string {
	switch t := typ.(type) {
	case nil:
		return "(an elided type)"
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok {
			return pkg.Name + "." + t.Sel.Name
		}
		return t.Sel.Name
	}
	return "(a type this reading cannot spell)"
}

// rewritesMapInPlace names the verb of a call that may rewrite its FIRST
// argument's map in place: the builtins `clear` and `delete`, and every
// function of the `maps` package. The ones that write take the destination
// first; one that only reads it answers here too, which is the side that
// shows the call rather than the side that hides it.
func rewritesMapInPlace(call *ast.CallExpr) (string, bool) {
	if len(call.Args) == 0 {
		return "", false
	}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if fun.Name == "clear" || fun.Name == "delete" {
			return fun.Name, true
		}
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "maps" {
			return "maps." + fun.Sel.Name, true
		}
	}
	return "", false
}

// buildsFromResolveVars reports whether an expression is a bare
// `…resolveVars(…)` call — the one builder of a run's vars. A wrapper
// CONTAINING such a call is a second builder, and is not this.
func buildsFromResolveVars(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "resolveVars"
}

// Source witness for the premise the whole reading rests on: a run's vars are
// built ONCE, by resolveVars, and never written again — so a `when` that reads
// nothing but them answers the same here and at the crossing. A per-key write,
// an in-place rewrite or a second builder reddens here rather than in a
// pre-flight that silently disagrees with the run.
//
// The domain is the engine's whole source, read as the compiler reads it
// (go/ast) and walked below pkg/runtime, the trees internal/treeskip names
// excepted: the top-level glob this started as never read
// pkg/runtime/recovery/, so a writer there would have landed unseen. The field
// is unexported, so no other package can write it.
//
// Four shapes write that map, and the rule is the same for each — the value is
// what resolveVars built, or it is named here with why it is that same map:
//
//   - `rs.vars = …`, which must be a bare `…resolveVars(…)` call;
//   - `rs.vars[k] = …` per key, and an in-place `clear`, `delete` or `maps.…`;
//   - a composite literal `runState{vars: …}`: the two legitimate ones hand on
//     a map built elsewhere, and say below which;
//   - an ALIAS the same function writes through (`m := rs.vars; m[k] = …`).
//
// A literal of another type keyed `vars` carries a map a run state already
// holds — the resolution scope every template read builds — and is named
// below: the type nobody classified reddens rather than passing as a state's.
//
// Same limit as its twin in pkg/runner
// (TestARunsLaunchInputsAreWrittenOnlyFromTheLaunchMap): a function the map is
// handed to and that writes through its own parameter, and an alias escaping
// its function, are beyond a reading of shapes.
func TestRunVarsAreBuiltOnceAndNeverWritten(t *testing.T) {
	// A run state whose vars a literal builds from something other than
	// resolveVars' answer, with why it is the same map all the same. The
	// right-hand side is part of the exception: a changed one is a new claim.
	exempt := map[string]string{
		// The throwaway state a contract check resolves against — the
		// checkpoint's vars, never the state the run's edges read: the
		// resumed run rebuilds its own from the run document's inputs.
		"resume.go": "cp.Vars",
		// A fan-out BRANCH's state, sharing the parent's map, which the
		// runState concurrency contract holds immutable for the fan-out.
		"parallel_checkpoint.go": "parent.vars",
	}
	// The other type carrying a field named `vars`, with what it is.
	otherVarsFields := map[string]string{
		"resolveScope": "the scope one template or expression resolution reads, built from a run state's own map",
	}
	root := filepath.Join("..", "..", "runtime")
	fset := token.NewFileSet()
	text := func(src []byte, e ast.Expr) string {
		from, to := fset.Position(e.Pos()).Offset, fset.Position(e.End()).Offset
		return strings.Join(strings.Fields(string(src[from:to])), " ")
	}
	built, scanned := 0, 0
	seen := map[string]int{}
	seenType := map[string]int{}
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if treeskip.Dir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		f, parseErr := parser.ParseFile(fset, path, src, 0)
		if parseErr != nil {
			return parseErr
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range node.Lhs {
					line := fset.Position(lhs.Pos()).Line
					if idx, isIndex := lhs.(*ast.IndexExpr); isIndex {
						if isRunVars(idx.X) {
							t.Errorf("%s:%d writes a run's vars per key — what the run reads is the map resolveVars built", rel, line)
						}
						continue
					}
					if !isRunVars(lhs) {
						continue
					}
					rhs := node.Rhs[0]
					if len(node.Rhs) == len(node.Lhs) {
						rhs = node.Rhs[i]
					}
					if !buildsFromResolveVars(rhs) {
						t.Errorf("%s:%d builds a run's vars from something other than a bare resolveVars(…): %s", rel, line, text(src, rhs))
						continue
					}
					built++
				}
			case *ast.CompositeLit:
				for _, el := range node.Elts {
					kv, isKeyed := el.(*ast.KeyValueExpr)
					if !isKeyed {
						continue
					}
					key, isIdent := kv.Key.(*ast.Ident)
					if !isIdent || key.Name != "vars" {
						continue
					}
					line := fset.Position(kv.Pos()).Line
					if !buildsARunState(node.Type) {
						typ := varsTypeName(node.Type)
						if _, named := otherVarsFields[typ]; !named {
							t.Errorf("%s:%d builds a %s with a `vars` this pin cannot place: if it is a run state's, build it from resolveVars; if it is another field of that name, name the type with what it is", rel, line, typ)
							continue
						}
						seenType[typ]++
						continue
					}
					switch rhsText := text(src, kv.Value); {
					case buildsFromResolveVars(kv.Value):
						built++
					case exempt[rel] == rhsText:
						seen[rel]++
					default:
						t.Errorf("%s:%d builds a run state's vars from %s — build them from resolveVars, or name this literal with why it carries the same map", rel, line, rhsText)
					}
				}
			case *ast.CallExpr:
				verb, inPlace := rewritesMapInPlace(node)
				if inPlace && isRunVars(node.Args[0]) {
					t.Errorf("%s:%d rewrites a run's vars in place (%s) — what the run reads is the map resolveVars built", rel, fset.Position(node.Pos()).Line, verb)
				}
			}
			return true
		})
		// An alias writes the same map without naming it, but only where the
		// function writes THROUGH it: the alias is collected from the
		// function's own body, and judged by what that body then does to it.
		for _, decl := range f.Decls {
			fd, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fd.Body == nil {
				continue
			}
			aliased := map[string]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				as, isAssign := n.(*ast.AssignStmt)
				if !isAssign || len(as.Lhs) != len(as.Rhs) {
					return true
				}
				for i, rhs := range as.Rhs {
					if !isRunVars(rhs) {
						continue
					}
					if id, isIdent := as.Lhs[i].(*ast.Ident); isIdent && id.Name != "_" {
						aliased[id.Name] = true
					}
				}
				return true
			})
			if len(aliased) == 0 {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range node.Lhs {
						idx, isIndex := lhs.(*ast.IndexExpr)
						if !isIndex {
							continue
						}
						if id, isIdent := idx.X.(*ast.Ident); isIdent && aliased[id.Name] {
							t.Errorf("%s:%d writes a run's vars per key through the alias %q", rel, fset.Position(lhs.Pos()).Line, id.Name)
						}
					}
				case *ast.CallExpr:
					verb, inPlace := rewritesMapInPlace(node)
					if !inPlace {
						return true
					}
					if id, isIdent := node.Args[0].(*ast.Ident); isIdent && aliased[id.Name] {
						t.Errorf("%s:%d rewrites a run's vars in place (%s) through the alias %q", rel, fset.Position(node.Pos()).Line, verb, id.Name)
					}
				}
				return true
			})
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	// The walk read the engine, not an empty tree: a premise nothing scanned
	// passes unchecked.
	if scanned < 50 {
		t.Fatalf("scanned %d non-test Go files under pkg/runtime — the walk no longer reads the engine this pin claims", scanned)
	}
	if built == 0 {
		t.Error("no `.vars = resolveVars(…)` found in the engine — the pin no longer reads what it claims")
	}
	for rel, rhs := range exempt {
		if seen[rel] != 1 {
			t.Errorf("%s: %d run-state literals matched the exception %q, want exactly 1 — a stale exception covers the next builder", rel, seen[rel], rhs)
		}
	}
	// A list naming a field this tree no longer carries is a list nobody
	// maintains, and the next type of that name would join it unread.
	for typ, what := range otherVarsFields {
		if seenType[typ] == 0 {
			t.Errorf("no literal builds a %s keyed `vars` — the list still describes it as %q", typ, what)
		}
	}
}

// Every router mode is classified deliberately: a new one must say whether its
// selection reads an edge's predicate, AND whether a matching guard pre-empts
// the guardless edges beside it — either read wrong lets the launch-vars
// filter drop an edge the engine takes. The classification may not contradict
// the walk's own reading either: a node that runs every branch reads no
// predicate to choose by.
func TestEdgeSelectionClassifiesEveryRouterMode(t *testing.T) {
	standard := edgeSelection{readsPredicates: true, fallbacksLoseToAGuard: true}
	want := map[RouterMode]edgeSelection{
		RouterCondition:  standard,
		RouterRoundRobin: {readsPredicates: true},
		RouterFanOutAll:  {},
		RouterFanOutEach: {},
		RouterLLM:        {},
	}
	for m := RouterMode(0); m.String() != "unknown"; m++ {
		sel, classified := want[m]
		if !classified {
			t.Fatalf("router mode %q is not classified: say whether its edge selection reads a `when`, and whether a guard pre-empts its guardless edges", m)
		}
		node := &RouterNode{BaseNode: BaseNode{ID: "r"}, RouterMode: m}
		if got := edgeSelectionAt(node); got != sel {
			t.Errorf("edgeSelectionAt(%q) = %+v, want %+v", m, got, sel)
		}
		if runsEveryBranch(node) && sel.readsPredicates {
			t.Errorf("router mode %q runs every branch yet is read as reading a predicate", m)
		}
	}
	// A node that is not a router takes the engine's standard selection.
	if got := edgeSelectionAt(&AgentNode{BaseNode: BaseNode{ID: "a"}}); got != standard {
		t.Errorf("an agent node = %+v, want the standard selection %+v", got, standard)
	}
	// A mode this reading does not know narrows nothing at all.
	if got := edgeSelectionAt(&RouterNode{BaseNode: BaseNode{ID: "r"}, RouterMode: RouterMode(99)}); got != (edgeSelection{}) {
		t.Errorf("an unclassified router mode = %+v, want no narrowing", got)
	}
}

package ir

import (
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// varsNamespace is the one expression namespace a launch answers for. Every
// other one — a node's outputs, an artifact, a loop or run counter, the
// implicit `input` a bare identifier reads — is decided while the run runs.
const varsNamespace = "vars"

// LaunchVarEdgeFilter is the EdgeFilter a pre-flight walk reads ONE RUN's
// topology through: the transitions this run's launch vars have already
// decided against are dropped, and everything else keeps the whole-graph
// answer. nil — no declared var, nothing decidable — reads exactly as the
// unfiltered walk.
//
// launch carries the run's launch values (queue.RunMessage.Vars), folded over
// the workflow's declared defaults as resolveVars stacks the two layers
// (launchVarsView, shared with the launch-time routing screen): that is the
// reading the run's own nodes and edges will make.
//
// An edge is dropped only where this answers the way the engine will, for
// EVERY crossing of it:
//
//   - its predicate is the parsed `when` Expression — the only form that can
//     read a var. The `Condition` field form names a field of the source
//     node's output, which no pre-flight holds;
//   - every reference it makes is a `vars.<name>` the folded view answers. An
//     absent var, a node output, an artifact, a loop or run reference, a bare
//     identifier, a drilled member the view does not carry, a value
//     resolveVars may rewrite under the run's workspace
//     (ValueHoldsAbsolutePath), a var whose text the environment answers
//     (varReadingConsultsEnv): each leaves the edge standing;
//   - it evaluates false with no error, under the same AST the engine
//     evaluates at the crossing (runtime.edgeWhenHolds). The VALUE is the same
//     at both readings — the map a run's vars are built from is never written
//     after resolveVars built it, and what is left in the view is text no
//     environment answers — so the predicate's answer does not change between
//     here and there.
//
// A node's FALLBACKS — its bare unconditional edge, its `else` — are dropped
// when one of its guarded siblings is decided TRUE: the first guard that
// matches is the crossing, and a fallback is taken only when none matched
// (runtime.evaluateEdgesWithLoopsRS), whatever order the edges were written
// in. That is the transition the usage-cap pre-flight was counting:
// review-pr's claude reviewer is `topology`'s unconditional fallback, so it
// read as reachable under a launch that had already selected the gpt family.
//
// Two forms of selection keep a fallback standing, because the engine still
// takes it there:
//
//   - an ITERATION back-edge whose `when` holds is declined at its cap, its
//     fuel, a liveness stall or its budget, and the exit beside it is the
//     crossing then — so it pre-empts nothing (IsBoundedIteration);
//   - a ROUND-ROBIN router cycles its guardless edges, reading no predicate
//     at all, so each is taken in turn whatever a guarded sibling decides
//     (edgeSelectionAt).
//
// A node may lose every exit. Both walks consume walkable's stopped
// annotation: CanReachVia crosses no excluded exit, and AlwaysReachesVia
// reads the node's own death as an end without a wall. A bounded iteration
// is not an exit once spent; its body remains reachable before exhaustion.
// Thus a C012-accepted iteration fallback does not turn a death into an
// endless cycle and discard the prune. A wall on the dying node itself
// still counts: the node executes before it tries to leave.
//
// The GRAPH refusal remains for a prune with neither a reachable terminal
// nor a node's own end (for example an unbounded, unannotated cycle). No
// such end means no evidence that a path avoids the walls, so the whole
// graph's answer stands.
//
// This narrows a walk; it never widens one. A caller asking "can this run
// avoid X" gets a smaller set of executions to avoid it in, so the answer is
// the run's own rather than the graph's.
func (w *Workflow) LaunchVarEdgeFilter(launch map[string]any) EdgeFilter {
	if w == nil {
		return nil
	}
	// The edge filter keeps its OWN probe for what a var's reading may
	// consult (varReadingConsultsEnv, below), not the view's late set: its
	// premise is that the answer can MOVE between two readings — which a
	// coercion failure's raw value answers differently again. The two
	// questions share launchVarsView and disagree on purpose.
	view, _ := launchVarsView(w, launch)
	for name := range view {
		if w.varReadingConsultsEnv(name, launch) {
			delete(view, name)
		}
	}
	if len(view) == 0 {
		return nil
	}
	outgoing := map[string][]*Edge{}
	for _, e := range w.Edges {
		if e != nil {
			outgoing[e.From] = append(outgoing[e.From], e)
		}
	}
	dropped := map[*Edge]bool{}
	for from, edges := range outgoing {
		src := w.Nodes[from]
		if src == nil {
			continue
		}
		sel := edgeSelectionAt(src)
		if !sel.readsPredicates {
			continue
		}
		var guardedHolds bool
		var excluded, fallbacks []*Edge
		for _, e := range edges {
			if e.Expression == nil {
				if e.Condition == "" {
					fallbacks = append(fallbacks, e)
				}
				continue
			}
			holds, decided := DecideOnLaunchVars(e.Expression, view)
			switch {
			case !decided:
			case holds:
				if !e.IsBoundedIteration() {
					guardedHolds = true
				}
			default:
				excluded = append(excluded, e)
			}
		}
		if !guardedHolds || !sel.fallbacksLoseToAGuard {
			fallbacks = nil
		}
		for _, e := range excluded {
			dropped[e] = true
		}
		for _, e := range fallbacks {
			dropped[e] = true
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	open := func(e *Edge) bool { return !dropped[e] }
	// Keep a prune that reaches either a terminal or the node's own end.
	// Only a topology with neither restores the whole-graph answer.
	if w.AlwaysReachesVia(open, noWall) {
		return nil
	}
	return open
}

// noWall accepts no node, so AlwaysReachesVia under it answers the one
// question that has nothing to do with a wall: does the entry still reach a
// terminal or a node's own end at all?
func noWall(Node) bool { return false }

// varReadingConsultsEnv reports whether reading this var's launch text
// CONSULTS the environment: an `${…}` form is answered by the process env and
// by the settings overlay (LookupEnv, repointable while a run waits), and the
// engine answers it AGAIN when it resolves the run's vars — after the clone,
// the sandbox start, or a capped window's reopening. Only a value whose text
// names nothing reads the same here and at the crossing, so a var reading any
// name decides nothing. The launch value, when there is one, is the text the
// run reads; the declared default otherwise, the layers launchVarsView stacks.
//
// Narrower than the engine-supplied-name omission launchVarsView applies for
// the routing screen: that one asks whether the screen's environment can
// answer the name at all, this one whether the answer can MOVE between two
// readings — which every name's can.
func (w *Workflow) varReadingConsultsEnv(name string, launch map[string]any) bool {
	v := w.Vars[name]
	if v == nil {
		return false
	}
	if raw, overridden := launch[name]; overridden {
		return consultsAnyEnvName(raw, v.Type)
	}
	return v.HasDefault && consultsAnyEnvName(v.Default, v.Type)
}

// DecideOnLaunchVars evaluates a `when` expression against a launch vars
// view alone, and reports whether that answer is the engine's: decided only
// when every reference is a `vars.<name>` the view answers with a value the
// run reads unchanged, and the evaluation returns without error.
//
// A predicate referencing nothing is not decided here: it reads no selection
// this view narrows, and its value is the compiler's business.
//
// Two readers share this one evaluation grammar: the edge filter's
// caller (this package) and the launch-time routing gate —
// model.FallbackWhenActive — whose `when:` routes the walk keeps or drops by
// it. Pass the view WITHOUT its late names: a name whose value the reading
// process decided is a reference the view cannot answer, and the gate holds
// what it guards undecided there. (The edge filter cleans its view by its
// own probe instead — varReadingConsultsEnv, a deliberately different
// question, which keeps the edge standing on undecided rather than holding
// anything; see its comment.)
func DecideOnLaunchVars(ast *expr.AST, view map[string]any) (holds, decided bool) {
	refs := ast.Refs()
	if len(refs) == 0 {
		return false, false
	}
	for _, r := range refs {
		if r.Namespace != varsNamespace || len(r.Path) == 0 {
			return false, false
		}
		val, found := drillVarsView(view, r.Path)
		if !found || holdsAbsolutePath(val) {
			return false, false
		}
	}
	// Only Vars is populated: a reference to any other namespace would read
	// nil or fail, and is refused above rather than answered with a blank.
	v, err := ast.EvalBool(&expr.Context{Vars: func(path []string) any {
		val, _ := drillVarsView(view, path)
		return val
	}})
	if err != nil {
		return false, false
	}
	return v, true
}

// ValueHoldsAbsolutePath reports whether a var's value carries an absolute
// filesystem path: itself, or in a leaf of the list or document it is.
//
// resolveVars REWRITES such a value where it names the repo root, to the
// worktree or in-container workspace the run works in (its foot-gun guard),
// and a pre-flight holds neither path: what the run reads at the crossing is
// then not what this view carries, so a reference reading one decides nothing.
//
// The three shapes are runtime.remapRepoRoot's own — a value it cannot walk
// into it does not rewrite either, so both readings agree on the rest.
// Exported for the twin a pkg/runtime test pins against that function.
func ValueHoldsAbsolutePath(v any) bool { return holdsAbsolutePath(v) }

func holdsAbsolutePath(v any) bool {
	switch val := v.(type) {
	case string:
		return filepath.IsAbs(val)
	case []any:
		for _, e := range val {
			if holdsAbsolutePath(e) {
				return true
			}
		}
	case map[string]any:
		for _, e := range val {
			if holdsAbsolutePath(e) {
				return true
			}
		}
	}
	return false
}

// edgeSelection is how a node chooses among its outgoing edges, in the only
// two terms a reading of the launch vars may narrow by.
type edgeSelection struct {
	// readsPredicates: the selection consults an outgoing edge's predicate, so
	// an edge the launch decided false is never taken.
	readsPredicates bool
	// fallbacksLoseToAGuard: a guardless edge is taken only when no guarded
	// sibling matched.
	fallbacksLoseToAGuard bool
}

// edgeSelectionAt classifies one node's edge selection. Every node that is not
// a router, and the `condition` router, take the engine's standard selection
// (runtime.evaluateEdgesWithLoopsRS): the first matching guard is the
// crossing, and the fallbacks are read only after.
//
// A mode this reading does not know narrows nothing — an unclassified
// selection may take any of its edges.
func edgeSelectionAt(n Node) edgeSelection {
	standard := edgeSelection{readsPredicates: true, fallbacksLoseToAGuard: true}
	r, ok := n.(*RouterNode)
	if !ok {
		return standard
	}
	switch r.RouterMode {
	case RouterCondition:
		return standard
	case RouterRoundRobin:
		// Cycles its GUARDLESS edges and reads no predicate
		// (runtime.execRoundRobin): a guarded edge is never taken there, and
		// the ones it cycles are taken in turn whatever a guard decides.
		return edgeSelection{readsPredicates: true}
	}
	// A fan-out runs every branch (prepareFanOut / execFanOutEach read no
	// predicate) and an `llm` router offers every outgoing target to the
	// model (execLLMRouter): no reading of the vars may drop one.
	return edgeSelection{}
}

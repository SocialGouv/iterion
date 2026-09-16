package dryrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Options tune a dry run.
type Options struct {
	// Fixtures answer the nodes they name (node id → output) in place of a
	// shape; a node without one is reported, and so is a key that names no
	// node. A key `node/child_node` answers a node of the child that subbot
	// node hands work to. nil: every output is a shape.
	Fixtures map[string]map[string]any
	// Inputs are launch values for vars; a var without a default and
	// without an input takes a shape of its type.
	Inputs map[string]any
	// WorkDir is the run's working directory; a dry run reads nothing there,
	// and an empty one is a temporary directory of its own — the operator's
	// place is never the run's.
	WorkDir string
	// Path is the main file the workflow was compiled from; a child's source
	// resolves beside it. Empty: children are not simulated, and said.
	Path string
	// Children resolves a `subbot source:` written in the file at parent to
	// the child's own path and compiled workflow; a nil function, or a nil
	// workflow with a nil error, leaves the child unsimulated — said. An
	// error is said with its reason. The child runs under the same bias,
	// its findings prefixed by the parent node.
	Children func(parent, source string) (path string, wf *ir.Workflow, err error)
	// Shell holds shell text to its parser; nil is Bash{}.
	Shell ShellChecker
	// Timeout bounds one pass, the children it simulates included — a
	// child runs under the node that hands it work, within what is left of
	// the pass; zero is a minute (`validate --exec-timeout` sets it). The
	// caller's context bounds the whole run. A pass that runs out of time is
	// said so (Pass.TimedOut), apart from a death of the program.
	Timeout time.Duration
}

// Edge names one traversal.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Pass is one execution of the graph under one bias.
type Pass struct {
	// Bias is the value every bool took and the enum end every enum took.
	Bias bool `json:"bias"`
	// Status is the run's terminal status as the store recorded it.
	Status string `json:"status"`
	// Failure is the engine's error, when it returned one.
	Failure string `json:"failure,omitempty"`
	// Deliberate says the pass ended at a fail node the bot declared — a
	// refusal the author wrote, not a death the dry run met.
	Deliberate bool `json:"deliberate,omitempty"`
	// TimedOut says the pass ran out of time — its deadline (Options.Timeout,
	// or the caller's context) expired before the run ended: not a death of
	// the program, the bound's; raise it.
	TimedOut bool `json:"timed_out,omitempty"`
	// Ceiling says the pass ran to a ceiling the dry run's shapes imposed —
	// the bot's budget max_iterations, the liveness monitor's stall on an
	// unbounded loop whose outputs never change under a shape, or the budget
	// guard that cannot fund another iteration: an exit
	// that rides a value the dry run shapes (a model's verdict) was never
	// met under this bias. Not a death the dry run can hold against the
	// bot, not a proof either: said as such.
	Ceiling bool `json:"ceiling,omitempty"`
	// Nodes are the nodes started, in order; Edges the edges selected.
	Nodes []string `json:"nodes"`
	Edges []Edge   `json:"edges"`
}

// ChildPass is one pass of a simulated child, under the node that handed
// it work — a path `node/child_node` past the first level.
type ChildPass struct {
	Node   string `json:"node"`
	Source string `json:"source"`
	Pass
}

// Report is what two passes met. Its JSON carries `clean` as well: the
// verdict of Clean.
type Report struct {
	Passes []Pass `json:"passes"`
	// Children are the passes of every child simulated, in node order then
	// bias: a child is read like the parent, its death is the parent's.
	Children []ChildPass `json:"children,omitempty"`
	// Findings, deduplicated across passes, by node.
	Findings []Finding `json:"findings,omitempty"`
	// Shaped lists the nodes whose output was a shape: a condition read
	// from one of them decided nothing about the real bot.
	Shaped []string `json:"shaped,omitempty"`
	// Pinned lists the nodes a fixture answered: a condition read from one
	// of them read the recording on both passes, not the bias.
	Pinned []string `json:"pinned,omitempty"`
	// UnvisitedNodes and UnvisitedEdges no pass reached — a child's under
	// its node's path.
	UnvisitedNodes []string `json:"unvisited_nodes,omitempty"`
	UnvisitedEdges []Edge   `json:"unvisited_edges,omitempty"`
}

// MarshalJSON adds the verdict, `clean`, to the report's fields.
func (r Report) MarshalJSON() ([]byte, error) {
	type plain Report
	return json.Marshal(struct {
		plain
		Clean bool `json:"clean"`
	}{plain(r), r.Clean()})
}

// coverage is what the passes reached of one program.
type coverage struct {
	wf    *ir.Workflow
	nodes map[string]bool
	edges map[Edge]bool
}

func newCoverage(wf *ir.Workflow) *coverage {
	return &coverage{wf: wf, nodes: map[string]bool{}, edges: map[Edge]bool{}}
}

func (c *coverage) saw(p Pass) {
	for _, id := range p.Nodes {
		c.nodes[id] = true
	}
	for _, e := range p.Edges {
		c.edges[e] = true
	}
}

// unvisited names what no pass reached, each name under prefix.
func (c *coverage) unvisited(prefix string) (nodes []string, edges []Edge) {
	for id, n := range c.wf.Nodes {
		if c.nodes[id] || implicitTerminal(id, n) {
			continue
		}
		nodes = append(nodes, prefix+id)
	}
	sort.Strings(nodes)
	listed := map[Edge]bool{}
	for _, e := range c.wf.Edges {
		if e == nil {
			continue
		}
		k := Edge{From: e.From, To: e.To}
		if c.edges[k] || listed[k] {
			continue
		}
		listed[k] = true
		edges = append(edges, Edge{From: prefix + e.From, To: prefix + e.To})
	}
	return nodes, edges
}

// Run executes wf twice — every bool true and every enum at its first
// value, then the other way — under a simulating engine, and reports what
// the passes met. Nothing reaches a model, a shell or the workspace: the
// executor answers, the waits are answered, the store is a temporary
// directory removed at the end, the run's worktree is none.
func Run(ctx context.Context, wf *ir.Workflow, opts Options) (*Report, error) {
	if wf == nil {
		return nil, errors.New("dryrun: no workflow")
	}
	shell := opts.Shell
	if shell == nil {
		shell = Bash{}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	r := &Report{}
	main := newCoverage(wf)
	children := map[string]*coverage{} // by the child's node path
	seenFindings := map[Finding]bool{}
	shaped := map[string]bool{}
	pinned := map[string]bool{}
	for _, bias := range []bool{true, false} {
		pass, x, err := runPass(ctx, wf, opts, shell, timeout, bias, 0)
		if err != nil {
			return nil, err
		}
		r.Passes = append(r.Passes, pass)
		main.saw(pass)
		for _, f := range x.Findings() {
			if !seenFindings[f] {
				seenFindings[f] = true
				r.Findings = append(r.Findings, f)
			}
		}
		for _, id := range x.Shaped() {
			shaped[id] = true
		}
		for _, id := range x.Pinned() {
			pinned[id] = true
		}
		for _, cr := range x.ChildRuns() {
			r.Children = append(r.Children, ChildPass{Node: cr.node, Source: cr.source, Pass: cr.pass})
			cov := children[cr.node]
			if cov == nil {
				cov = newCoverage(cr.wf)
				children[cr.node] = cov
			}
			cov.saw(cr.pass)
		}
	}
	sortFindings(r.Findings)
	sort.SliceStable(r.Children, func(i, j int) bool {
		if r.Children[i].Node != r.Children[j].Node {
			return r.Children[i].Node < r.Children[j].Node
		}
		return r.Children[i].Bias && !r.Children[j].Bias
	})
	for id := range shaped {
		r.Shaped = append(r.Shaped, id)
	}
	sort.Strings(r.Shaped)
	for id := range pinned {
		r.Pinned = append(r.Pinned, id)
	}
	sort.Strings(r.Pinned)
	r.UnvisitedNodes, r.UnvisitedEdges = main.unvisited("")
	childNodes := make([]string, 0, len(children))
	for node := range children {
		childNodes = append(childNodes, node)
	}
	sort.Strings(childNodes)
	for _, node := range childNodes {
		nodes, edges := children[node].unvisited(node + "/")
		r.UnvisitedNodes = append(r.UnvisitedNodes, nodes...)
		r.UnvisitedEdges = append(r.UnvisitedEdges, edges...)
	}
	return r, nil
}

// maxChildDepth bounds the simulation of children of children.
const maxChildDepth = 4

// runPass runs the graph once under one bias; depth counts the subbot
// levels above this one.
func runPass(ctx context.Context, wf *ir.Workflow, opts Options, shell ShellChecker, timeout time.Duration, bias bool, depth int) (Pass, *Executor, error) {
	pass := Pass{Bias: bias}
	dir, err := os.MkdirTemp("", "iterion-dryrun-")
	if err != nil {
		return pass, nil, err
	}
	defer os.RemoveAll(dir)
	st, err := store.New(filepath.Join(dir, "store"))
	if err != nil {
		return pass, nil, err
	}
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = filepath.Join(dir, "work")
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return pass, nil, err
		}
	}
	// The dry run never opens a worktree: a shallow copy carries the choice,
	// the caller's workflow is left as compiled.
	sim := *wf
	sim.Worktree = "none"
	x := NewExecutor(&sim, bias, opts.Fixtures, shell)
	x.path = opts.Path
	x.children = opts.Children
	if depth < maxChildDepth {
		x.simulate = func(ctx context.Context, child *ir.Workflow, path, node string) (Pass, *Executor, error) {
			childOpts := opts
			childOpts.Path = path
			childOpts.Fixtures = childFixtures(opts.Fixtures, node)
			return runPass(ctx, child, childOpts, shell, timeout, bias, depth+1)
		}
	}
	var mu sync.Mutex
	var declined string
	observe := func(evt store.Event) {
		mu.Lock()
		defer mu.Unlock()
		switch evt.Type {
		case store.EventBudgetWarning:
			// The engine says why it declined a loop edge: the reason of
			// the decline the pass ended on is what its end is read by
			// (ceilingOf) — a ceiling has no edge selected after it.
			if reason, _ := evt.Data["reason"].(string); reason != "" {
				declined = reason
			}
		case store.EventNodeStarted:
			pass.Nodes = append(pass.Nodes, evt.NodeID)
		case store.EventEdgeSelected:
			// The run moved past any decline: what it dies of later is
			// that node's, not the ceiling's.
			declined = ""
			from, _ := evt.Data["from"].(string)
			to, _ := evt.Data["to"].(string)
			pass.Edges = append(pass.Edges, Edge{From: from, To: to})
		case store.EventBranchStarted:
			// A fan-out activates its branches without an edge_selected: the
			// engine names the edge that started the branch, and that edge
			// alone is recorded — never every edge into the entry node, which
			// would credit a router no pass reached.
			from, _ := evt.Data["from"].(string)
			pass.Edges = append(pass.Edges, Edge{From: from, To: evt.NodeID})
		}
	}
	eng := runtime.New(&sim, st, x,
		runtime.WithSimulation(runtime.Simulation{AnswerHumans: true, EventsArrive: true, AnswersArrive: true}),
		runtime.WithEventObserver(observe),
		runtime.WithSandboxOverride("none"),
		runtime.WithWorkDir(workDir),
		runtime.WithSubbotRunner(x.subbotRunner()),
		runtime.WithLogger(iterlog.Nop()),
	)
	runID, err := store.GenerateRunID()
	if err != nil {
		return pass, nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	runErr := eng.Run(rctx, runID, launchInputs(wf, opts.Inputs, bias))
	if runErr != nil {
		pass.Failure = runErr.Error()
		if rctx.Err() != nil {
			pass.TimedOut = true
		}
	}
	x.fixtureKeys()
	if run, err := st.LoadRun(ctx, runID); err == nil && run != nil {
		pass.Status = string(run.Status)
		mu.Lock()
		pass.Ceiling = ceilingOf(run.FailureCode, declined)
		mu.Unlock()
	} else if runErr != nil {
		pass.Status = "error"
	}
	mu.Lock()
	defer mu.Unlock()
	if n := len(pass.Nodes); n > 0 {
		if _, ok := wf.Nodes[pass.Nodes[n-1]].(*ir.FailNode); ok {
			pass.Deliberate = true
		}
	}
	return pass, x, nil
}

// ceilingOf says a run's end was a ceiling the dry run's shapes imposed
// rather than a death of the program: the bot's own budget ceiling, or —
// when the engine declined a loop edge for a reason that is a ceiling's
// (the liveness monitor on unchanging shapes, the budget guard, an
// unbounded loop out of fuel) and no edge was left — the fall-through's
// death. declined is that decline's reason, or empty once the run moved
// past it. A bounded loop declined at its cap (`loop_cap`) is the
// program's: C145's shape, a death.
func ceilingOf(code store.FailureCode, declined string) bool {
	if code == store.FailureBudgetExceeded {
		return true
	}
	switch declined {
	case "liveness_stall", "loop_budget_guard", "loop_out_of_fuel":
		return code == store.FailureNoOutgoingEdge || code == store.FailureLoopExhausted
	}
	return false
}

// implicitTerminal reports the `done` and `fail` every workflow carries
// unwritten: a `fail` no pass reached is the good outcome, not a gap. A
// fail node the author declared keeps its name and is listed when unvisited.
func implicitTerminal(id string, n ir.Node) bool {
	switch n.(type) {
	case *ir.DoneNode:
		return id == "done"
	case *ir.FailNode:
		return id == "fail"
	}
	return false
}

// launchInputs is what the launch supplies: the caller's inputs, and a
// shape for every var without a default the caller left out.
func launchInputs(wf *ir.Workflow, given map[string]any, bias bool) map[string]any {
	inputs := map[string]any{}
	for k, v := range given {
		inputs[k] = v
	}
	for name, v := range wf.Vars {
		if _, ok := inputs[name]; ok || v == nil || v.HasDefault {
			continue
		}
		inputs[name] = VarValue(v, bias)
	}
	return inputs
}

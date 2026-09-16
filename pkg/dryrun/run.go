package dryrun

import (
	"context"
	"errors"
	"os"
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
	// shape; a node without one is reported. nil: every output is a shape.
	Fixtures map[string]map[string]any
	// Inputs are launch values for vars; a var without a default and
	// without an input takes a shape of its type.
	Inputs map[string]any
	// WorkDir is the run's working directory; a dry run reads nothing there.
	WorkDir string
	// Shell holds shell text to its parser; nil is Bash{}.
	Shell ShellChecker
	// Timeout bounds one pass; zero is a minute.
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
	// Nodes are the nodes started, in order; Edges the edges selected.
	Nodes []string `json:"nodes"`
	Edges []Edge   `json:"edges"`
}

// Report is what two passes met.
type Report struct {
	Passes []Pass `json:"passes"`
	// Findings, deduplicated across passes, by node.
	Findings []Finding `json:"findings"`
	// Shaped lists the nodes whose output was a shape: a condition read
	// from one of them decided nothing about the real bot.
	Shaped []string `json:"shaped"`
	// UnvisitedNodes and UnvisitedEdges no pass reached.
	UnvisitedNodes []string `json:"unvisited_nodes"`
	UnvisitedEdges []Edge   `json:"unvisited_edges"`
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
	seenNodes := map[string]bool{}
	seenEdges := map[Edge]bool{}
	seenFindings := map[Finding]bool{}
	shaped := map[string]bool{}
	for _, bias := range []bool{true, false} {
		pass, x, err := runPass(ctx, wf, opts, shell, timeout, bias)
		if err != nil {
			return nil, err
		}
		r.Passes = append(r.Passes, pass)
		for _, f := range x.Findings() {
			if !seenFindings[f] {
				seenFindings[f] = true
				r.Findings = append(r.Findings, f)
			}
		}
		for _, id := range x.Shaped() {
			shaped[id] = true
		}
		for _, id := range pass.Nodes {
			seenNodes[id] = true
		}
		for _, e := range pass.Edges {
			seenEdges[e] = true
		}
	}
	sortFindings(r.Findings)
	for id := range shaped {
		r.Shaped = append(r.Shaped, id)
	}
	sort.Strings(r.Shaped)
	for id, n := range wf.Nodes {
		if seenNodes[id] || implicitTerminal(id, n) {
			continue
		}
		r.UnvisitedNodes = append(r.UnvisitedNodes, id)
	}
	sort.Strings(r.UnvisitedNodes)
	listed := map[Edge]bool{}
	for _, e := range wf.Edges {
		if e == nil {
			continue
		}
		k := Edge{From: e.From, To: e.To}
		if seenEdges[k] || listed[k] {
			continue
		}
		listed[k] = true
		r.UnvisitedEdges = append(r.UnvisitedEdges, k)
	}
	return r, nil
}

// runPass runs the graph once under one bias.
func runPass(ctx context.Context, wf *ir.Workflow, opts Options, shell ShellChecker, timeout time.Duration, bias bool) (Pass, *Executor, error) {
	pass := Pass{Bias: bias}
	dir, err := os.MkdirTemp("", "iterion-dryrun-")
	if err != nil {
		return pass, nil, err
	}
	defer os.RemoveAll(dir)
	st, err := store.New(dir)
	if err != nil {
		return pass, nil, err
	}
	// The dry run never opens a worktree: a shallow copy carries the choice,
	// the caller's workflow is left as compiled.
	sim := *wf
	sim.Worktree = "none"
	x := NewExecutor(&sim, bias, opts.Fixtures, shell)
	var mu sync.Mutex
	observe := func(evt store.Event) {
		mu.Lock()
		defer mu.Unlock()
		switch evt.Type {
		case store.EventNodeStarted:
			pass.Nodes = append(pass.Nodes, evt.NodeID)
		case store.EventEdgeSelected:
			from, _ := evt.Data["from"].(string)
			to, _ := evt.Data["to"].(string)
			pass.Edges = append(pass.Edges, Edge{From: from, To: to})
		}
	}
	eng := runtime.New(&sim, st, x,
		runtime.WithSimulation(runtime.Simulation{AnswerHumans: true, EventsArrive: true, AnswersArrive: true}),
		runtime.WithEventObserver(observe),
		runtime.WithSandboxOverride("none"),
		runtime.WithWorkDir(opts.WorkDir),
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
	}
	if run, err := st.LoadRun(ctx, runID); err == nil && run != nil {
		pass.Status = string(run.Status)
	} else if runErr != nil {
		pass.Status = "error"
	}
	mu.Lock()
	defer mu.Unlock()
	return pass, x, nil
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

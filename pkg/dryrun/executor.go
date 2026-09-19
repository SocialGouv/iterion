package dryrun

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// Kind classifies what a dry run met.
type Kind string

const (
	// KindUnresolvedRef: a `{{…}}` the renderer kept as written — the node
	// would have sent the placeholder to the model or the shell.
	KindUnresolvedRef Kind = "unresolved_ref"
	// KindShellSyntax: the interpreter refuses the rendered text.
	KindShellSyntax Kind = "shell_syntax"
	// KindUnchecked: something a dry run has no reading of — an interpreter
	// without a checker, a connector action, a subbot child — said, never
	// guessed.
	KindUnchecked Kind = "unchecked"
	// KindNoFixture: fixtures were supplied and this node has none, so its
	// output is a shape.
	KindNoFixture Kind = "no_fixture"
	// KindFixture: a fixture does not fit the node's output schema — the
	// production validator's word, the one a real run would apply — or
	// names no node of the program.
	KindFixture Kind = "fixture"
	// KindInconclusive: an expression failed while reading a value the dry
	// run invented — a `json` field it shaped, a `json` var no launch value
	// filled, a value derived from one — so the failure decided nothing
	// about the program. The finding names the value and what would decide
	// it; the field it computed reads as a shape in turn, and the pass goes
	// on. Not a defect: Report.Failing reads past it, Report.Clean does not.
	KindInconclusive Kind = "inconclusive"
)

// Finding is one thing the dry run met, at a node.
type Finding struct {
	Node   string `json:"node"`
	Kind   Kind   `json:"kind"`
	Where  string `json:"where,omitempty"`
	Detail string `json:"detail"`
}

func (f Finding) String() string {
	if f.Where == "" {
		return fmt.Sprintf("%s [%s]: %s", f.Node, f.Kind, f.Detail)
	}
	return fmt.Sprintf("%s [%s] %s: %s", f.Node, f.Kind, f.Where, f.Detail)
}

// Executor is the NodeExecutor of a dry run: no backend, no shell. It
// renders what the node would send — its prompts, its command, its script,
// its postcondition — through the production renderers, holds shell text to
// the interpreter's parser, and answers with the node's fixture or a
// schema-shaped output. What it meets is a Finding, never an error: the run
// goes on, so one pass covers the graph.
type Executor struct {
	wf       *ir.Workflow
	bias     bool
	fixtures map[string]map[string]any
	shell    ShellChecker
	// iterated[nodeID][field] holds an output field a downstream iteration
	// reads (a fan_out_each `over:`, a foreach, a lambda combinator's
	// collection): a `json` field there is shaped as a one-element list.
	// Computed once at construction, so a node's output shape is the same
	// on every crossing of a loop.
	iterated map[string]map[string]bool
	// given are the launch values the caller supplied for vars: the
	// program's own values, never invented (Options.Inputs).
	given map[string]any
	// path is the main file this workflow came from; children resolves a
	// child's source beside it; simulate runs a child under the node that
	// hands it work (nil at the depth cap).
	path     string
	children func(parent, source string) (string, *ir.Workflow, error)
	simulate func(ctx context.Context, child *ir.Workflow, path, node string) (Pass, *Executor, error)
	// childRuns are the children this pass simulated, grandchildren
	// included, each under the path of nodes that reached it.
	childRuns []childRun
	// childMemos are the children simulated, one per node: a subbot inside
	// a loop is crossed many times, and under the shapes every crossing
	// hands the child the same work — its pass is simulated on the first
	// crossing and counted on the others, so the report and the time a
	// pass takes are bounded by the program, not by its loops.
	childMemos map[string]*childMemo

	mu       sync.Mutex
	vars     map[string]any
	workDir  string
	executed []string
	shaped   []string
	pinned   []string
	findings []Finding
	// undecided[nodeID][field] holds the compute fields whose expression
	// could not be decided in this pass: their value is a shape, invented
	// in turn (Inconclusive).
	undecided map[string]map[string]bool
}

// declaredSecrets resolves a declared secret to a placeholder — the value a
// real run's guard puts there is not the dry run's to know — and an
// undeclared one to nothing, which the resolver then reports.
type declaredSecrets struct{ wf *ir.Workflow }

func (d declaredSecrets) ResolveSecretRef(name string) string {
	if d.wf == nil || d.wf.Secrets[name] == nil {
		return ""
	}
	return "<secret:" + name + ">"
}

// childRun is one simulated child: the node that handed it work (a path
// `node/child_node` past the first level), its source and file, its
// program, and how its pass went.
type childRun struct {
	node, source, path string
	wf                 *ir.Workflow
	pass               Pass
	// crossings counts the times the node handed the child work: the
	// pass is the first crossing's, the others are the same work.
	crossings int
}

// childMemo is one child's simulation, shared by every crossing of its
// node: done is closed once the first crossing has its pass.
type childMemo struct {
	done chan struct{}
}

// ChildRuns are the children this pass simulated, grandchildren included.
func (x *Executor) ChildRuns() []childRun {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]childRun(nil), x.childRuns...)
}

// NewExecutor builds the executor of one pass over wf. bias decides the
// shape of a bool or an enum; fixtures, when given, answer the nodes they
// name; shell holds shell text (nil: unchecked, and said). The iterations
// of every output field are walked once here — a `json` field a
// downstream iteration reads takes the one-element list shape.
func NewExecutor(wf *ir.Workflow, bias bool, fixtures map[string]map[string]any, shell ShellChecker) *Executor {
	return &Executor{wf: wf, bias: bias, fixtures: fixtures, shell: shell, iterated: iteratedFields(wf)}
}

// SetVars receives the run's vars from the engine (its varsSetter seam),
// as the production executor does.
func (x *Executor) SetVars(vars map[string]any) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.vars == nil {
		x.vars = make(map[string]any, len(vars))
	}
	for k, v := range vars {
		x.vars[k] = v
	}
}

// SetWorkDir receives the run's working directory (the engine's
// workDirSetter seam); a dry run reads nothing there.
func (x *Executor) SetWorkDir(dir string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.workDir = dir
}

// Findings are what the pass met, in order.
func (x *Executor) Findings() []Finding {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]Finding(nil), x.findings...)
}

// Executed lists the nodes the engine handed to the executor, in order.
func (x *Executor) Executed() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]string(nil), x.executed...)
}

// Shaped lists the nodes whose output was a shape, not a fixture.
func (x *Executor) Shaped() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]string(nil), x.shaped...)
}

// Pinned lists the nodes a fixture answered: every condition read from one
// of them read the recording, not the pass's bias.
func (x *Executor) Pinned() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]string(nil), x.pinned...)
}

// shapedTemplateData is the engine's template data with a shape for every
// declared attachment the dry run has no file for: a declared attachment
// is not an unresolved reference. The engine's maps are read-only here; the
// copy carries its own.
func (x *Executor) shapedTemplateData(td *model.TemplateData) *model.TemplateData {
	if td == nil || len(x.wf.Attachments) == 0 {
		return td
	}
	cp := *td
	cp.Attachments = make(map[string]model.AttachmentInfo, len(td.Attachments)+len(x.wf.Attachments))
	for k, v := range td.Attachments {
		cp.Attachments[k] = v
	}
	for name := range x.wf.Attachments {
		if _, ok := cp.Attachments[name]; !ok {
			// A dry run wires no signer: without a shape here `.url` would
			// resolve to nothing, and a declared attachment would read as an
			// undeclared one.
			shaped := name
			cp.Attachments[name] = model.AttachmentInfo{
				Name:       name,
				Path:       "<attachment:" + name + ">",
				MIME:       "application/octet-stream",
				PresignURL: func() (string, error) { return "<attachment-url:" + shaped + ">", nil },
			}
		}
	}
	return &cp
}

func (x *Executor) add(f Finding) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.findings = append(x.findings, f)
}

// Execute implements runtime.NodeExecutor.
func (x *Executor) Execute(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	id := node.NodeID()
	td := x.shapedTemplateData(model.TemplateDataFromContext(ctx))
	x.mu.Lock()
	x.executed = append(x.executed, id)
	vars := x.vars
	x.mu.Unlock()
	runID := ""
	if td != nil {
		runID = td.RunID
	}
	schema := ""
	switch n := node.(type) {
	case *ir.AgentNode:
		x.prompt(id, "system prompt", n.SystemPrompt, input, vars, td)
		x.prompt(id, "user prompt", n.UserPrompt, input, vars, td)
		schema = n.OutputSchema
	case *ir.JudgeNode:
		x.prompt(id, "system prompt", n.SystemPrompt, input, vars, td)
		x.prompt(id, "user prompt", n.UserPrompt, input, vars, td)
		schema = n.OutputSchema
	case *ir.RouterNode:
		if n.RouterMode != ir.RouterLLM {
			// A condition router routes on what it received.
			return input, nil
		}
		x.prompt(id, "system prompt", n.SystemPrompt, input, vars, td)
		x.prompt(id, "user prompt", n.UserPrompt, input, vars, td)
		if n.RouterMulti {
			// A multi-select llm router is read on `selected_routes`: every
			// candidate on the true pass — the widest fan-out, every branch
			// covered — the last one alone on the false pass.
			return map[string]any{"selected_routes": x.routes(id)}, nil
		}
		return map[string]any{"selected_route": x.route(id)}, nil
	case *ir.HumanNode:
		x.prompt(id, "instructions", n.Instructions, input, vars, td)
		x.prompt(id, "system prompt", n.SystemPrompt, input, vars, td)
		schema = n.OutputSchema
	case *ir.ToolNode:
		schema = n.OutputSchema
		switch {
		case n.Action != "":
			x.add(Finding{Node: id, Kind: KindUnchecked, Where: "action", Detail: fmt.Sprintf("connector action %s is not executed by a dry run: its output is a shape", n.Action)})
		case n.Script != "":
			rendered := model.RenderScript(n.Script, n.ScriptRefs, input, vars, td, runID, x.reporter(id, "script"))
			x.shellCheck(id, "script", n.Language, rendered)
		default:
			rendered := model.RenderCommand(n.Command, n.CommandRefs, input, vars, td, runID, x.reporter(id, "command"))
			x.shellCheck(id, "command", "bash", rendered)
		}
		if n.Postcondition != "" {
			rendered := model.RenderCommand(n.Postcondition, n.PostcondRefs, input, vars, td, runID, x.reporter(id, "postcondition"))
			x.shellCheck(id, "postcondition", "bash", rendered)
		}
	default:
		x.add(Finding{Node: id, Kind: KindUnchecked, Detail: fmt.Sprintf("%s node: a dry run has no reading of it beyond the shape of its output", node.NodeKind())})
	}
	return x.output(id, schema), nil
}

// prompt renders a prompt the node names and reports each reference the
// renderer kept as written.
func (x *Executor) prompt(id, where, name string, input, vars map[string]any, td *model.TemplateData) {
	if name == "" {
		return
	}
	p := x.wf.Prompts[name]
	if p == nil {
		x.add(Finding{Node: id, Kind: KindUnchecked, Where: where, Detail: fmt.Sprintf("prompt %q is not declared", name)})
		return
	}
	r := &model.TemplateResolver{Vars: vars, Secrets: declaredSecrets{x.wf}, Unresolved: func(ref string) {
		x.unresolved(id, where, ref)
	}}
	r.Resolve(p.Body, input, td)
}

// unresolved reports a reference kept as written. A declared secret never
// reaches it: the prompt resolver renders it as a placeholder
// (declaredSecrets) and the command renderer as the guard's placeholder.
func (x *Executor) unresolved(id, where, ref string) {
	x.add(Finding{Node: id, Kind: KindUnresolvedRef, Where: where, Detail: fmt.Sprintf("{{%s}} resolves to nothing here: %s", ref, x.whyUnresolved(ref))})
}

// reporter is the renderer's listener for one place of a node: each
// reference the renderer resolved to nothing is a finding there. The
// rendered text is never re-read for braces — a value may carry `{{…}}` of
// its own, and that is the value, not a reference.
func (x *Executor) reporter(id, where string) func(ref string) {
	return func(ref string) { x.unresolved(id, where, ref) }
}

// shellCheck holds rendered shell text to its interpreter's parser.
func (x *Executor) shellCheck(id, where, interpreter, text string) {
	if x.shell == nil {
		x.add(Finding{Node: id, Kind: KindUnchecked, Where: where, Detail: "shell syntax not checked: no checker"})
		return
	}
	err := x.shell.Check(interpreter, text)
	switch {
	case err == nil:
	case errors.Is(err, ErrCheckTimeout):
		x.add(Finding{Node: id, Kind: KindUnchecked, Where: where, Detail: err.Error()})
	case errors.Is(err, ErrNoChecker):
		lang := interpreter
		if lang == "" {
			lang = "sh"
		}
		x.add(Finding{Node: id, Kind: KindUnchecked, Where: where, Detail: fmt.Sprintf("%s: no syntax check for this interpreter (%v)", lang, err)})
	default:
		x.add(Finding{Node: id, Kind: KindShellSyntax, Where: where, Detail: err.Error()})
	}
}

// candidates are the targets an llm router may select: its outgoing edges.
func (x *Executor) candidates(id string) []string {
	var out []string
	for _, e := range x.wf.Edges {
		if e != nil && e.From == id {
			out = append(out, e.To)
		}
	}
	return out
}

// route is a single-select LLM router's choice: the first outgoing target on
// the true pass, the last on the false one — both are candidates the engine
// accepts.
func (x *Executor) route(id string) string {
	candidates := x.candidates(id)
	if len(candidates) == 0 {
		return ""
	}
	if x.bias {
		return candidates[0]
	}
	return candidates[len(candidates)-1]
}

// routes are a multi-select LLM router's choice: every candidate on the true
// pass, the last one alone on the false pass.
func (x *Executor) routes(id string) []any {
	candidates := x.candidates(id)
	if len(candidates) == 0 {
		return []any{}
	}
	if x.bias {
		out := make([]any, len(candidates))
		for i, c := range candidates {
			out[i] = c
		}
		return out
	}
	return []any{candidates[len(candidates)-1]}
}

// output is the node's fixture when one is given, else a shape of its
// output schema.
func (x *Executor) output(id, schema string) map[string]any {
	if x.fixtures != nil {
		if fx, ok := x.fixtures[id]; ok {
			out := make(map[string]any, len(fx))
			for k, v := range fx {
				out[k] = v
			}
			x.mu.Lock()
			x.pinned = append(x.pinned, id)
			x.mu.Unlock()
			// The production validator's word on the recording: a fixture
			// that does not fit the schema would not have come out of a
			// real run of this node.
			if sch := x.wf.Schemas[schema]; schema != "" && sch != nil {
				if err := model.ValidateOutput(out, sch); err != nil {
					x.add(Finding{Node: id, Kind: KindFixture, Detail: "the fixture does not fit the node's output schema: " + err.Error()})
				}
			}
			return out
		}
		if schema != "" {
			x.add(Finding{Node: id, Kind: KindNoFixture, Detail: "no fixture for this node: its output is a shape"})
		}
	}
	var sch *ir.Schema
	if schema != "" {
		sch = x.wf.Schemas[schema]
		x.mu.Lock()
		x.shaped = append(x.shaped, id)
		x.mu.Unlock()
	}
	return SynthesizeAt(sch, x.bias, x.iterated[id])
}

// fixtureKeys reports a fixture that names no node of this program: a
// misspelled node would otherwise read as a thin recording. A key
// `node/child_node` addresses a node of the child that subbot node hands
// work to, and is that child's pass to check.
func (x *Executor) fixtureKeys() {
	keys := make([]string, 0, len(x.fixtures))
	for key := range x.fixtures {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := x.wf.Nodes[key]; ok {
			continue
		}
		if head, _, isChild := strings.Cut(key, "/"); isChild {
			if _, ok := x.wf.Nodes[head].(*ir.SubbotNode); ok {
				continue
			}
		}
		x.add(Finding{Node: key, Kind: KindFixture, Detail: "the fixture names no node of this program"})
	}
}

// childFixtures are the fixtures addressed to the child under node —
// the keys `node/…` with the prefix removed.
func childFixtures(fixtures map[string]map[string]any, node string) map[string]map[string]any {
	var out map[string]map[string]any
	for key, fx := range fixtures {
		rest, ok := strings.CutPrefix(key, node+"/")
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]map[string]any{}
		}
		out[rest] = fx
	}
	return out
}

// subbotRunner answers a subbot node in this pass. With a child loader and
// a path, the child is read beside the parent and simulated under the same
// bias: its pass is carried on the report's children, its findings, shapes
// and pins prefixed by the node (`node/child_node`); without one — or past
// the depth cap — the child is not simulated, said. Either way the parent
// receives a shape of the schema it declared for the child's output: what a
// child's terminal node produces is the child's business, and a shape is
// what the parent's contract to it promises.
func (x *Executor) subbotRunner() runtime.SubbotRunner {
	return func(ctx context.Context, req runtime.SubbotRequest) (map[string]any, error) {
		schema := ""
		if sb, ok := x.wf.Nodes[req.NodeID].(*ir.SubbotNode); ok {
			schema = sb.OutputSchema
		}
		switch {
		case strings.HasPrefix(req.Source, "bot://"):
			x.add(Finding{Node: req.NodeID, Kind: KindUnchecked, Where: "subbot", Detail: fmt.Sprintf("child %s is a registry bot: not simulated, its output is a shape", req.Source)})
		case x.children == nil || x.path == "" || x.simulate == nil:
			x.add(Finding{Node: req.NodeID, Kind: KindUnchecked, Where: "subbot", Detail: fmt.Sprintf("child %s is not simulated in this pass: its output is a shape", req.Source)})
		default:
			path, child, err := x.children(x.path, req.Source)
			switch {
			case err != nil:
				x.add(Finding{Node: req.NodeID, Kind: KindUnchecked, Where: "subbot", Detail: fmt.Sprintf("child %s not simulated: %v", req.Source, err)})
			case child == nil:
				x.add(Finding{Node: req.NodeID, Kind: KindUnchecked, Where: "subbot", Detail: fmt.Sprintf("child %s not read: its output is a shape", req.Source)})
			default:
				x.mu.Lock()
				if x.childMemos == nil {
					x.childMemos = map[string]*childMemo{}
				}
				memo, crossed := x.childMemos[req.NodeID]
				if !crossed {
					memo = &childMemo{done: make(chan struct{})}
					x.childMemos[req.NodeID] = memo
				}
				x.mu.Unlock()
				if crossed {
					// The same work again: the pass simulated on the first
					// crossing stands, and this crossing is counted on it.
					<-memo.done
					x.mu.Lock()
					for i := range x.childRuns {
						if x.childRuns[i].node == req.NodeID {
							x.childRuns[i].crossings++
						}
					}
					x.mu.Unlock()
					break
				}
				// Under the node's own context: the child runs within what is
				// left of the parent's pass, never on a budget of its own.
				pass, cx, err := x.simulate(ctx, child, path, req.NodeID)
				close(memo.done)
				if err != nil {
					x.add(Finding{Node: req.NodeID, Kind: KindUnchecked, Where: "subbot", Detail: fmt.Sprintf("child %s could not be simulated: %v", req.Source, err)})
					break
				}
				for _, f := range cx.Findings() {
					f.Node = req.NodeID + "/" + f.Node
					x.add(f)
				}
				shaped, pinned, runs := cx.Shaped(), cx.Pinned(), cx.ChildRuns()
				x.mu.Lock()
				for _, id := range shaped {
					x.shaped = append(x.shaped, req.NodeID+"/"+id)
				}
				for _, id := range pinned {
					x.pinned = append(x.pinned, req.NodeID+"/"+id)
				}
				x.childRuns = append(x.childRuns, childRun{node: req.NodeID, source: req.Source, path: path, wf: child, pass: pass, crossings: 1})
				for _, cr := range runs {
					cr.node = req.NodeID + "/" + cr.node
					x.childRuns = append(x.childRuns, cr)
				}
				x.mu.Unlock()
			}
		}
		return x.output(req.NodeID, schema), nil
	}
}

// whyUnresolved says, for a reference kept as written, what a dry run can
// say about why — from the program it holds, never from the text alone: a
// declared attachment that resolves to nothing is not an undeclared one.
func (x *Executor) whyUnresolved(ref string) string {
	ns, rest, _ := strings.Cut(ref, ".")
	switch ns {
	case "attachments":
		name, sub, _ := strings.Cut(rest, ".")
		if x.wf != nil && x.wf.Attachments[name] != nil {
			if sub == "" {
				sub = "path"
			}
			return fmt.Sprintf("the attachment is declared, but its %q has no value here", sub)
		}
		return "no such attachment is declared"
	}
	return whyUnresolvedNamespace(ns)
}

// whyUnresolvedNamespace is the reading a namespace alone allows.
func whyUnresolvedNamespace(ns string) string {
	switch ns {
	case "input":
		return "the node's input carries no such field on this path (the edge that reached it maps none)"
	case "vars":
		return "no such var is declared"
	case "outputs":
		return "that node has produced nothing on this path yet, or its output has no such field"
	case "loop":
		return "no such loop is declared (a declared loop's counters resolve even outside its body)"
	case "each":
		return "the foreach binding resolves on an edge's `with:` mapping, not in a prompt or a tool body: map the item onto the node's input and read {{input.…}}"
	case "artifacts":
		return "nothing was published under that name before this node"
	case "secrets":
		return "no such secret is declared"
	case "run":
		return "the run namespace has no such member"
	default:
		return "unknown namespace"
	}
}

// sortFindings orders findings by node, then place, then text.
func sortFindings(fs []Finding) {
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].Node != fs[j].Node {
			return fs[i].Node < fs[j].Node
		}
		if fs[i].Where != fs[j].Where {
			return fs[i].Where < fs[j].Where
		}
		return fs[i].Detail < fs[j].Detail
	})
}

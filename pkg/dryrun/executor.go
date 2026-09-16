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

	mu       sync.Mutex
	vars     map[string]any
	workDir  string
	executed []string
	shaped   []string
	findings []Finding
}

// NewExecutor builds the executor of one pass over wf. bias decides the
// shape of a bool or an enum; fixtures, when given, answer the nodes they
// name; shell holds shell text (nil: unchecked, and said).
func NewExecutor(wf *ir.Workflow, bias bool, fixtures map[string]map[string]any, shell ShellChecker) *Executor {
	return &Executor{wf: wf, bias: bias, fixtures: fixtures, shell: shell}
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

func (x *Executor) add(f Finding) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.findings = append(x.findings, f)
}

// Execute implements runtime.NodeExecutor.
func (x *Executor) Execute(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	id := node.NodeID()
	td := model.TemplateDataFromContext(ctx)
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
			rendered := model.RenderScript(n.Script, n.ScriptRefs, input, vars, td, runID)
			x.leftovers(id, "script", rendered)
			x.shellCheck(id, "script", n.Language, rendered)
		default:
			rendered := model.RenderCommand(n.Command, n.CommandRefs, input, vars, td, runID)
			x.leftovers(id, "command", rendered)
			x.shellCheck(id, "command", "bash", rendered)
		}
		if n.Postcondition != "" {
			rendered := model.RenderCommand(n.Postcondition, n.PostcondRefs, input, vars, td, runID)
			x.leftovers(id, "postcondition", rendered)
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
	r := &model.TemplateResolver{Vars: vars, Unresolved: func(ref string) {
		x.add(Finding{Node: id, Kind: KindUnresolvedRef, Where: where, Detail: fmt.Sprintf("{{%s}} resolves to nothing here: %s", ref, whyUnresolved(ref))})
	}}
	r.Resolve(p.Body, input, td)
}

// leftovers reports the references a rendered command or script still
// carries — the production renderers keep a reference that resolves to
// nothing as written, so the shell sees it.
func (x *Executor) leftovers(id, where, rendered string) {
	rest := rendered
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			return
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			return
		}
		ref := strings.TrimSpace(rest[i+2 : i+j])
		if ref != "" && ref != ir.LiteralOpenExpression {
			ref = strings.TrimPrefix(ref, "!")
			x.add(Finding{Node: id, Kind: KindUnresolvedRef, Where: where, Detail: fmt.Sprintf("{{%s}} resolves to nothing here: %s", ref, whyUnresolved(ref))})
		}
		rest = rest[i+j+2:]
	}
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

// route is the LLM router's choice: the first outgoing target on the true
// pass, the last on the false one — both are candidates the engine accepts.
func (x *Executor) route(id string) string {
	var candidates []string
	for _, e := range x.wf.Edges {
		if e != nil && e.From == id {
			candidates = append(candidates, e.To)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	if x.bias {
		return candidates[0]
	}
	return candidates[len(candidates)-1]
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
	return Synthesize(sch, x.bias)
}

// subbotRunner answers a subbot node in this pass: the child is not
// simulated here — said — and the parent receives a shape of the schema it
// declared for the child's output.
func (x *Executor) subbotRunner() runtime.SubbotRunner {
	return func(_ context.Context, req runtime.SubbotRequest) (map[string]any, error) {
		x.add(Finding{Node: req.NodeID, Kind: KindUnchecked, Where: "subbot", Detail: fmt.Sprintf("child %s is not simulated in this pass: its output is a shape", req.Source)})
		schema := ""
		if sb, ok := x.wf.Nodes[req.NodeID].(*ir.SubbotNode); ok {
			schema = sb.OutputSchema
		}
		return x.output(req.NodeID, schema), nil
	}
}

// whyUnresolved says, for a reference kept as written, what a dry run can
// say about why.
func whyUnresolved(ref string) string {
	ns, _, _ := strings.Cut(ref, ".")
	switch ns {
	case "input":
		return "the node's input carries no such field on this path (the edge that reached it maps none)"
	case "vars":
		return "no such var is declared"
	case "outputs":
		return "that node has produced nothing on this path yet, or its output has no such field"
	case "loop":
		return "the node is not inside that loop here"
	case "artifacts":
		return "nothing was published under that name before this node"
	case "attachments":
		return "no such attachment"
	case "secrets":
		return "no secret guard in a dry run"
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

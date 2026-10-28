package dryrun

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/subbotcontracts"
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
	// KindUnmappedOnPath: an `{{input.X}}` the renderer kept as written
	// whose field ANOTHER incoming edge maps — a loop's back-edge, a
	// sibling `when` path — so it is legitimately empty on the flagged path
	// only. A warning: Failing reads past it (the never-mapped case stays
	// KindUnresolvedRef, and stays a --strict failure).
	KindUnmappedOnPath Kind = "unmapped_on_path"
	// KindEnvRead: the braced environment forms of a tool body, and what the
	// preview answered each with — the `${NAME:-default}` reads it resolved
	// to the author's default and the plain `${NAME}` reads it kept as
	// written (ADR-130). The recall half of C188: the reads a compile-time
	// check cannot judge without lying — a name only the engine's own
	// environment supplies — are said here as a preview FACT instead of
	// guessed at authoring time. An observation, never a defect: a
	// deliberate dial read is the legitimate shape of exactly this fact, so
	// Failing reads past it and a `--strict` gate stays green on it.
	KindEnvRead Kind = "env_read"
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

	mu   sync.Mutex
	vars map[string]any
	// program answers the three seams the engine's admission reads — the
	// tool surface, the backend and a node's `fallbacks:` routes — from the
	// dry run's own vars. Guarded by mu,
	// built on the first question and rebuilt after SetVars changes them;
	// one handed out is never written again.
	program  *model.ClawExecutor
	workDir  string
	executed []string
	shaped   []string
	pinned   []string
	findings []Finding
	// produced holds every node the pass saw finish — simulated nodes and
	// the engine-internal ones (computes, routers) alike, from the event
	// stream. A producer absent from it has output nothing on this pass: a
	// read of its output failed on absence, not on a shape (Inconclusive).
	produced map[string]bool
	// What the pass observed of each declared loop's crossings: how many
	// edges of the name fired, and the source of the latest and of the
	// previous one. `loop.<name>.previous_output` is the snapshot of the
	// crossing BEFORE the latest (the first crossing stages only the
	// current output), taken from the edge the pass selected — the consult
	// reads this instead of guessing from the workflow's edge list.
	loopCrossings map[string]int
	loopPrevSrc   map[string]string
	loopLastSrc   map[string]string
	// undecided[nodeID][field] holds the compute fields whose expression
	// could not be decided in this pass: their value is a shape, invented
	// in turn (Inconclusive).
	undecided map[string]map[string]bool
	// cutShort holds the bounded loops whose back-edge the SIMULATION
	// declined past its crossing bound — the dry run's doing, never the
	// program's (Pass.LoopsCutShort carries the names; a death that follows
	// is a ceiling, runtime.CeilingReason).
	cutShort map[string]bool
}

// markProduced records a node the pass saw finish.
func (x *Executor) markProduced(node string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.produced == nil {
		x.produced = map[string]bool{}
	}
	x.produced[node] = true
}

// hasProduced reports whether the node finished on this pass.
func (x *Executor) hasProduced(node string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.produced[node]
}

// recordLoopCrossing mirrors the engine's loop bookkeeping for one selected
// loop edge: each crossing rotates the previous snapshot's source behind the
// latest one. No reset here — the engine resets a loop's counter on a
// NON-loop edge entering a body node from outside the body
// (recordTrunkEdge), and this method only ever sees loop edges:
// `ir.Loop.Entries` is by construction the set of the loop-bearing
// back-edges' targets, so testing it here fired on every crossing and
// pinned the crossings at one (PR #1491 review R651a94).
func (x *Executor) recordLoopCrossing(name, from, to string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.loopCrossings == nil {
		x.loopCrossings = map[string]int{}
		x.loopPrevSrc = map[string]string{}
		x.loopLastSrc = map[string]string{}
	}
	x.loopCrossings[name]++
	x.loopPrevSrc[name] = x.loopLastSrc[name]
	x.loopLastSrc[name] = from
}

// recordTrunkEdge mirrors the engine's loop re-entry for one selected
// non-loop edge: entering a loop's body from outside it, at one of the
// loop's entries, resets the loop's crossings — the engine drops the
// snapshots with the counter, so the previous_output story starts over.
func (x *Executor) recordTrunkEdge(from, to string) {
	if x.wf == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for name, loop := range x.wf.Loops {
		if loop == nil || len(loop.Body) == 0 || loop.Body[from] || !loop.Body[to] {
			continue
		}
		if x.loopCrossings[name] > 0 && loop.Entries[to] {
			x.loopCrossings[name] = 0
			x.loopPrevSrc[name] = ""
			x.loopLastSrc[name] = ""
		}
	}
}

// recordLoopCutShort names a bounded loop the SIMULATION declined past its
// crossing bound — the dry run's doing, never the program's. The pass
// carries the names (Pass.LoopsCutShort); a death that follows is a
// ceiling (runtime.CeilingReason), not the program's LOOP_EXHAUSTED.
func (x *Executor) recordLoopCutShort(loop string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.cutShort == nil {
		x.cutShort = map[string]bool{}
	}
	x.cutShort[loop] = true
}

// loopsCutShort are the loops the pass cut short, sorted for the report.
func (x *Executor) loopsCutShort() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.cutShort) == 0 {
		return nil
	}
	out := make([]string, 0, len(x.cutShort))
	for name := range x.cutShort {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// previousOutputSource names the node whose output the loop's
// previous_output snapshot holds, and whether a snapshot exists: the first
// crossing stages only the current output, so the snapshot appears with the
// second.
func (x *Executor) previousOutputSource(name string) (string, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.loopCrossings[name] < 2 || x.loopPrevSrc[name] == "" {
		return "", false
	}
	return x.loopPrevSrc[name], true
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
	// answer is the projected shape the first crossing returned, so every
	// crossing of the same subbot node answers the same value.
	answer map[string]any
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
	x.program = nil
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
		// The run's own declarations: a dry run that rendered a `json` var
		// as argv words would report a command the run never issues.
		shapes := model.WorkflowShapes(x.wf).WithInputSchema(x.wf.Schemas[n.InputSchema])
		switch {
		case n.Action != "":
			x.add(Finding{Node: id, Kind: KindUnchecked, Where: "action", Detail: fmt.Sprintf("connector action %s is not executed by a dry run: its output is a shape", n.Action)})
		case n.Script != "":
			body, reads := x.bodyEnv(n, n.Script)
			x.envReading(id, "script", reads)
			rendered := model.RenderScript(body, n.ScriptRefs, input, vars, td, runID, x.reporter(id, "script", input))
			x.shellCheck(id, "script", n.Language, rendered, reads)
		default:
			body, reads := x.bodyEnv(n, n.Command)
			x.envReading(id, "command", reads)
			rendered := model.RenderCommand(body, n.CommandRefs, input, vars, td, runID, shapes, x.reporter(id, "command", input))
			x.shellCheck(id, "command", "bash", rendered, reads)
		}
		if n.Postcondition != "" {
			body, reads := x.bodyEnv(n, n.Postcondition)
			x.envReading(id, "postcondition", reads)
			rendered := model.RenderCommand(body, n.PostcondRefs, input, vars, td, runID, shapes, x.reporter(id, "postcondition", input))
			x.shellCheck(id, "postcondition", "bash", rendered, reads)
		}
	default:
		x.add(Finding{Node: id, Kind: KindUnchecked, Detail: fmt.Sprintf("%s node: a dry run has no reading of it beyond the shape of its output", node.NodeKind())})
	}
	return x.output(id, schema), nil
}

// bodyEnv is the preview's reading of a tool body's environment forms: the
// reading the run gives it (model.ToolBodyEnv), under a lookup that answers
// nothing. A `${NAME:-default}` resolves to its default — the answer the run
// uses when the engine's environment does not carry the name — and a plain
// `${NAME}` is kept as written, because whether it is answered depends on
// the ENGINE's process, which the validating process is not: reading this
// host's environment would make the verdict follow the laptop. It is the
// reading `validate` already gives a launch value (ir.ResolveVarTextAsWritten,
// #1607), extended to the three bodies the engine hands a reader.
//
// The braced forms the reading consulted travel with the body: they are what
// attributes a syntax refusal the preview's own answer may have caused
// (bracedDefaultAttribution).
func (x *Executor) bodyEnv(n *ir.ToolNode, body string) (string, []ir.BracedEnvRead) {
	return model.ToolBodyEnv(n, body, nil)
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
		x.unresolved(id, where, ref, input)
	}}
	r.Resolve(p.Body, input, td)
}

// unresolved reports a reference kept as written. A declared secret never
// reaches it: the prompt resolver renders it as a placeholder
// (declaredSecrets) and the command renderer as the guard's placeholder.
// A reference the consult proves to rest on a value the dry run invented —
// an item a fan-out drew from a shaped collection — is inconclusive, not a
// defect: the render names the value and what would decide it, and the
// verdict reads it the way it reads every undecided expression.
// An `{{input.…}}` kept as written is classified first (inputRefFinding):
// mapped by no incoming edge stays the unresolved_ref it always was — a
// --strict failure; mapped by another incoming edge is the path-only kind,
// a warning (#1455).
func (x *Executor) unresolved(id, where, ref string, input map[string]any) {
	if why, ok := x.inventedRenderRef(ref); ok {
		x.add(Finding{Node: id, Kind: KindInconclusive, Where: where, Detail: fmt.Sprintf("{{%s}} renders a shape: %s", ref, why)})
		return
	}
	ns, rest, _ := strings.Cut(ref, ".")
	if ns == "input" {
		x.add(x.inputRefFinding(id, where, ref, rest, input))
		return
	}
	x.add(Finding{Node: id, Kind: KindUnresolvedRef, Detail: fmt.Sprintf("{{%s}} resolves to nothing here: %s", ref, x.whyUnresolved(ref)), Where: where})
}

// inventedRenderRef answers whether a rendered reference rests on a value
// the dry run invented: the ref reads `namespace.path…`, and only the
// namespaces the provenance walk knows answer. A nodeless render ref reads
// the producer the ref itself names.
func (x *Executor) inventedRenderRef(ref string) (string, bool) {
	ns, rest, _ := strings.Cut(ref, ".")
	if ns == "" || rest == "" {
		return "", false
	}
	return x.invented("", expr.Ref{Namespace: ns, Path: strings.Split(rest, ".")}, nil)
}

// reporter is the renderer's listener for one place of a node: each
// reference the renderer resolved to nothing is a finding there. The
// rendered text is never re-read for braces — a value may carry `{{…}}` of
// its own, and that is the value, not a reference.
func (x *Executor) reporter(id, where string, input map[string]any) func(ref string) {
	return func(ref string) { x.unresolved(id, where, ref, input) }
}

// inputRefFinding classifies an `{{input.X}}` the renderer kept as written
// (#1455), from the input the pass actually handed the node and from the
// node's incoming edges:
//
//   - the top-level field is absent from the input and NO incoming edge
//     maps it — never mapped, empty on every path: the unresolved_ref it
//     always was, a --strict failure;
//   - absent from the input and ANOTHER incoming edge maps it — empty on
//     the flagged path only (a loop's back-edge, a sibling `when` path):
//     the path-only kind, a warning;
//   - present in the input but the reference drills deeper than the value
//     carries — the mapping delivered, the shape under it decides: still an
//     unresolved_ref, worded with that fact (the old wording, "the edge
//     that reached it maps none", was false in this case).
//
// A node with no incoming edges at all — the entry's shape alone, or a
// node a pass never reached — keeps the generic wording: nothing static to
// say, the run-level inputs may carry the field.
func (x *Executor) inputRefFinding(id, where, ref, rest string, input map[string]any) Finding {
	path := strings.Split(rest, ".")
	if len(path) == 0 || path[0] == "" {
		return x.genericUnresolved(id, where, ref)
	}
	top := path[0]
	if _, ok := input[top]; ok {
		// The field arrived; the reference died deeper than the mapping
		// delivered. inputRefFinding is also asked about {{input}} with no
		// field at all — same arm.
		if len(path) == 1 {
			return x.genericUnresolved(id, where, ref)
		}
		return Finding{
			Node: id, Kind: KindUnresolvedRef, Where: where,
			Detail: fmt.Sprintf("{{%s}} resolves to nothing here: the input carries %q on this path, but no %q beneath it", ref, top, strings.Join(path[1:], ".")),
		}
	}
	var mappedBy, silent []string
	for _, e := range x.wf.Edges {
		if e == nil || e.To != id {
			continue
		}
		maps := false
		for _, dm := range e.With {
			if dm.Key == top {
				maps = true
				break
			}
		}
		name := edgeName(e)
		if maps {
			mappedBy = append(mappedBy, name)
		} else {
			silent = append(silent, name)
		}
	}
	switch {
	case len(mappedBy)+len(silent) == 0:
		// No incoming edge at all: the entry's shape or a node a pass never
		// reached — the run-level inputs may carry the field, nothing static
		// to say.
		return x.genericUnresolved(id, where, ref)
	case len(mappedBy) == 0:
		// Never mapped by any incoming edge: empty on every path, a certain
		// defect — the unresolved_ref it always was.
		quoted(&silent)
		return Finding{
			Node: id, Kind: KindUnresolvedRef, Where: where,
			Detail: fmt.Sprintf("{{%s}} resolves to nothing here: no incoming edge of this node maps %q — empty on every path (incoming: %s)", ref, top, strings.Join(silent, ", ")),
		}
	default:
		quoted(&mappedBy)
		if len(silent) == 0 {
			// EVERY incoming edge maps the field: nothing structural left to
			// name, the emptiness is temporal — the mapping edge has not
			// delivered on this crossing (a loop's first pass, its head
			// entered before the back-edge ever fired).
			return Finding{
				Node: id, Kind: KindUnmappedOnPath, Where: where,
				Detail: fmt.Sprintf("{{%s}} is empty on this path: mapped by %s — empty before their delivery on this crossing (a loop's first pass)", ref, strings.Join(mappedBy, ", ")),
			}
		}
		quoted(&silent)
		return Finding{
			Node: id, Kind: KindUnmappedOnPath, Where: where,
			Detail: fmt.Sprintf("{{%s}} is empty on this path: mapped by %s; %s maps none", ref, strings.Join(mappedBy, ", "), strings.Join(silent, ", ")),
		}
	}
}

// quoted wraps each name in backquotes, for an edge named in a finding.
func quoted(names *[]string) {
	for i, n := range *names {
		(*names)[i] = "`" + n + "`"
	}
}

// edgeName is how a finding names an edge: from -> to, with the condition
// that guards it — two parallel edges between one pair of nodes (a `when`
// path and its sibling) must not read as one.
func edgeName(e *ir.Edge) string {
	name := e.From + " -> " + e.To
	switch {
	case e.Condition != "":
		cond := e.Condition
		if e.Negated {
			cond = "not " + cond
		}
		name += " (when " + cond + ")"
	case e.ExpressionSrc != "":
		name += " (when " + e.ExpressionSrc + ")"
	case e.IsElse:
		name += " (else)"
	}
	return name
}

// genericUnresolved is the wording that says only what this path saw: the
// field is absent and nothing static contradicts it.
func (x *Executor) genericUnresolved(id, where, ref string) Finding {
	return Finding{Node: id, Kind: KindUnresolvedRef, Where: where, Detail: fmt.Sprintf("{{%s}} resolves to nothing here: %s", ref, whyUnresolvedNamespace("input"))}
}

// shellCheck holds rendered shell text to its interpreter's parser. reads are
// the braced env forms the body's reading consulted, which attribute a syntax
// refusal the preview's own answer may have caused; nil where the text is not
// a tool body.
func (x *Executor) shellCheck(id, where, interpreter, text string, reads []ir.BracedEnvRead) {
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
		x.add(Finding{Node: id, Kind: KindShellSyntax, Where: where, Detail: err.Error() + bracedDefaultAttribution(reads)})
	}
}

// bracedDefaultAttribution names the answer the PREVIEW gave, when a body the
// interpreter refuses carried a `${NAME:-default}`: the preview resolves that
// arm to the author's default, so the refused text is the reading for an
// engine whose environment does not carry the name — an engine whose process
// does carry it hands the reader its value instead, and that text is not this
// one. Without the clause the operator reads a bare parser error on a body
// that parses as written. Empty where no `:-` arm was read: a plain `${NAME}`
// is kept as written, so the preview's text is the author's there.
func bracedDefaultAttribution(reads []ir.BracedEnvRead) string {
	names := bracedArmForms(reads, true)
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf(" — the preview answered %s with the author's default: this is the text an engine whose environment does not carry the name hands the reader, and an engine that carries it runs its value instead", strings.Join(names, ", "))
}

// envReading reports, per body, what the preview answered the body's braced
// environment forms with: the `${NAME:-default}` reads it resolved to the
// author's default — which a run whose engine environment carries the name
// answers with that value instead — and the plain `${NAME}` reads it kept as
// written, which only the engine's own process decides. It is the decision
// record ADR-130 asks the preview to REPORT rather than guess, and it is the
// recall half of C188: the read a compile-time check cannot judge without
// lying on a deliberate dial becomes a fact an author reads before paying for
// a run.
//
// reads are the chokepoint's own record (model.ToolBodyEnv's second return
// value), never a second scan of the body: a second reader would answer for
// an environment the body never went through, and a node that DECLINED the
// reading (`env_expansion: none`) hands back no read at all — so its body is
// reported as having had nothing answered, because nothing was. Empty where
// the body carries no form the engine consults: there is no decision to
// report where none was made.
func (x *Executor) envReading(id, where string, reads []ir.BracedEnvRead) {
	if len(reads) == 0 {
		return
	}
	var clauses []string
	if forms := bracedArmForms(reads, true); len(forms) > 0 {
		clauses = append(clauses, fmt.Sprintf("%s resolved to the author's default (a run whose engine environment carries the name runs its value instead)", strings.Join(forms, ", ")))
	}
	if forms := bracedArmForms(reads, false); len(forms) > 0 {
		clauses = append(clauses, fmt.Sprintf("%s kept as written (only the engine's own process answers a plain read, and the validating process is not it)", strings.Join(forms, ", ")))
	}
	x.add(Finding{Node: id, Kind: KindEnvRead, Where: where, Detail: fmt.Sprintf(
		"the engine reads this body's environment forms before any reader sees the text, and the preview read them under an environment that answers no name: %s. `env_expansion: none` on the node keeps every form verbatim",
		strings.Join(clauses, "; "))})
}

// bracedArmForms are the forms of ONE arm of a body's reading, in body order,
// each name once: `${NAME:-…}` for the arm the engine resolves
// unconditionally (hasDefault), `${NAME}` for the one its own process
// decides. A name read twice in the same arm is one form here — the report
// names WHAT was answered, and the authored body is where the positions are
// (ir.BracedEnvRead.At).
func bracedArmForms(reads []ir.BracedEnvRead, hasDefault bool) []string {
	var forms []string
	seen := map[string]bool{}
	for _, r := range reads {
		if r.HasDefault != hasDefault || seen[r.Name] {
			continue
		}
		seen[r.Name] = true
		if hasDefault {
			forms = append(forms, "${"+r.Name+":-…}")
		} else {
			forms = append(forms, "${"+r.Name+"}")
		}
	}
	return forms
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
					answer := memo.answer
					x.mu.Unlock()
					if answer != nil {
						return answer, nil
					}
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
				// A contracted child projects its answer from its contract
				// ports — the same subbotcontracts.ProjectOutput shape the
				// runtime reads — each port's producer shape synthesized
				// from the producer's own schema; the parent's declared
				// schema remains the coarse validator of what comes back
				// (#1535).
				if projected := subbotContractShape(child, &pass, x.bias, x.iterated); len(projected) > 0 {
					x.mu.Lock()
					memo.answer = projected
					x.mu.Unlock()
					return projected, nil
				}
			}
		}
		return x.output(req.NodeID, schema), nil
	}
}

// subbotContractShape projects the answer a contracted child gives from
// its contract ports — the same subbotcontracts.ProjectOutput shape the
// runtime reads — with each port's producer output synthesized from the
// producer's own schema, a shape like every dry-run value. A producer no
// pass reached projects nothing, the runtime's own rule (its capture
// holds what ran). nil when the child keeps no contract: the parent's
// declared schema stays the coarse answer.
func subbotContractShape(child *ir.Workflow, pass *Pass, bias bool, iterated map[string]map[string]bool) map[string]any {
	if child == nil || child.Contract == nil || len(child.Contract.Outputs) == 0 {
		return nil
	}
	ran := make(map[string]bool, len(pass.Nodes))
	for _, id := range pass.Nodes {
		ran[id] = true
	}
	nodeShapes := map[string]map[string]any{}
	for _, p := range child.Contract.Outputs {
		if p == nil || p.FromNode == "" || !ran[p.FromNode] {
			continue
		}
		if _, done := nodeShapes[p.FromNode]; done {
			continue
		}
		node := child.Nodes[p.FromNode]
		if node == nil {
			continue
		}
		var sch *ir.Schema
		if name := ir.NodeOutputSchema(node); name != "" {
			sch = child.Schemas[name]
		}
		nodeShapes[p.FromNode] = SynthesizeAt(sch, bias, iterated[p.FromNode])
	}
	return subbotcontracts.ProjectOutput(child.Contract, nodeShapes)
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
	case "vars":
		// The same distinction the attachment case makes, for the same
		// reason. A var the dry run left unsupplied — no default, and no
		// value of the shapes this package produces satisfies its
		// `[matching: ...]` pattern — is not an undeclared one. And an
		// undeclared {{vars.X}} is a compile error (C033), so it never
		// reaches a dry run at all: without this arm the message below
		// would be false every time it was printed.
		name, _, _ := strings.Cut(rest, ".")
		v := varOf(x.wf, name)
		switch {
		case v == nil:
			return "no such var is declared"
		case !v.HasDefault && v.Matching != "":
			// The only way a declared var reaches here today: the seeding
			// left it out because no shape it produces satisfies the
			// pattern. Said precisely, because a message that names a
			// cause it did not check becomes false the day another one
			// appears.
			return "the var is declared with no default, and the dry run could not invent a value its [matching: ...] pattern admits — pass one with --var"
		default:
			return "the var is declared, but it has no value on this path"
		}
	}
	return whyUnresolvedNamespace(ns)
}

// varOf is the workflow's declaration of a var, or nil when the workflow
// is absent or declares none by that name.
func varOf(wf *ir.Workflow, name string) *ir.Var {
	if wf == nil {
		return nil
	}
	return wf.Vars[name]
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

// EffectiveToolNames answers the engine's tool-surface seam for a dry run.
//
// The engine's parallel-branch guard asks its executor which tools a node will
// actually hold, because the runtime folds its own opt-ins over the author's
// `tools:` list at build time (`auto_memory:` grants a file writer on claw,
// ultracode grants the subagent tool). An executor that answers short leaves
// the guard reading the declaration, so `iterion validate --exec --strict`
// would call clean a fan-out `iterion run` refuses at the router; one that
// cannot answer is read at the worst case — every agent and judge not marked
// `readonly:` counts as writing — so the dry run would refuse fan-outs a run
// admits. Either way two products of one tree would disagree about one file.
//
// The answer is not re-derived here — a second copy of the append rules is the
// thing this seam exists to remove — but it is asked of a PROGRAM-ONLY
// executor. A real one resolves its opt-ins through the host too
// (ITERION_AUTO_MEMORY, ITERION_DEFAULT_BACKEND, a credential probe), and a
// static check that read those would give one file two verdicts on two
// machines, and refuse a file a run with `--auto-memory off` admits. A dry run
// judges the program; a run is judged again by the engine, against what that
// run holds.
//
// The appends are decided per ROUTE — `auto_memory: on` grants write_file on
// claw and nothing on codex — so this answer is a union over the backends the
// node may run on, and those are THIS dry run's reading of them (routeBackends),
// never the program executor's second reading of the same routes: that one
// resolves a route's `when:` gate from the value this package invented for an
// unsupplied var, and a surface taken over it dropped claw's appends from a node
// whose admission counted the claw route — `validate --exec` clean on a fan-out
// `--var rescue=on` has the engine refuse at the router.
func (x *Executor) EffectiveToolNames(node ir.Node, mayEscalateToUltracode bool) []string {
	return x.programExecutor().EffectiveToolNamesOnRoutes(node, mayEscalateToUltracode, x.routeBackends(node))
}

// routeBackends is this dry run's ONE reading of the backends a node may run
// on, for every question that needs the set: the primary its backend seam
// answers, then each route its fallback seam answers that is not that primary.
//
// Both halves are the answers the engine's admission reads, so a question
// answered from this list cannot disagree with the verdict taken on it — the
// drift this file's seams exist to remove, one package further in.
func (x *Executor) routeBackends(node ir.Node) []string {
	// Both seams below read the node, so the nil arm the tool surface documents
	// ("nothing to add", the declared list) is answered here rather than
	// resolved: this is the one reader a nil node reaches.
	if node == nil {
		return nil
	}
	primary := x.EffectiveBackendName(node)
	out := []string{primary}
	for _, route := range x.EffectiveFallbackBackends(node) {
		if route != primary {
			out = append(out, route)
		}
	}
	return out
}

// EffectiveBackendName answers the engine's backend seam for a dry run, from
// the same program-only executor, given the dry run's vars — the launch values
// and the program's own defaults, as for the rest of the program: the node's
// backend, else the workflow's, `{{vars.…}}` resolved from those vars, and ""
// where the program names neither — the IR is all there is, which is what the
// guard read before this seam was asked of a dry run. An executor that
// cannot answer is read at the worst case, so without it the dry run would
// refuse fan-outs of agents and judges not marked `readonly:` wherever a node
// declaring a write tool would be refused.
//
// On a launch this program does not fully specify, a TEMPLATED routing field is
// answered by the author's own spelling instead — the same reading, and the
// same reason, as the fallback seam below it (launchDecidesTheProgramsRoutes):
// a template is the one part of a routing field the LAUNCH decides, the value
// this package seeded for an undecided var is no run's, and admission reads an
// unresolved spelling as the route it is. It is also the only name the author
// can act on; the shape this package seeds ("x" for a string) appears nowhere
// in their program.
func (x *Executor) EffectiveBackendName(node ir.Node) string {
	if authored := x.authoredBackendTheLaunchDecides(node); authored != "" {
		return authored
	}
	return x.programExecutor().EffectiveBackendName(node)
}

// authoredBackendTheLaunchDecides is the spelling that decides where a node
// runs on a launch the program does not fully specify — and "" where the
// program's own values decide the field, or where the launch decides every var.
//
// It reads the two fields dispatch reads, in dispatch's order
// (model.ClawExecutor.resolveBackendName steps 1-2): the node's own `backend:`,
// then the workflow's `default_backend:`. A spelling that RESOLVES to a route
// decides the node's route there and the workflow default is never reached;
// every other one may leave the route to that default — absent, `auto`, or a
// reference that resolves to nothing — so the default is read wherever the
// node's own field could reach it.
func (x *Executor) authoredBackendTheLaunchDecides(node ir.Node) string {
	if x.launchDecidesTheProgramsRoutes() {
		return ""
	}
	own := strings.TrimSpace(nodeBackendField(node))
	if fieldIsTemplated(own) {
		return own
	}
	if x.routeSpellingResolvesToARoute(own) {
		return ""
	}
	if x.wf == nil {
		return ""
	}
	if d := strings.TrimSpace(x.wf.DefaultBackend); fieldIsTemplated(d) {
		return d
	}
	return ""
}

// nodeBackendField is a node's own `backend:` spelling, read off the three
// kinds dispatch reads one from (model.ClawExecutor.resolveBackendName) — a
// kind that declares none routes through the workflow default alone.
func nodeBackendField(node ir.Node) string {
	switch n := node.(type) {
	case *ir.AgentNode:
		return n.Backend
	case *ir.JudgeNode:
		return n.Backend
	case *ir.RouterNode:
		return n.Backend
	}
	return ""
}

// routeSpellingResolvesToARoute reports whether a `backend:` spelling names the
// node's route once this dry run's own vars and environment are read: not
// nothing, and not the explicit `auto` — the two spellings dispatch resolves
// from elsewhere (ir.BackendFallsThrough, the ONE spelling of that rule). It is
// the reading dispatch takes before it consults the workflow default, asked of
// the resolver that takes it (ResolveRoutingField) and not of the syntax.
//
// It is asked of the spellings the launch does not decide — a name, a `${VAR}`
// reference, `auto`, an absent field: a template is answered before it, by the
// author's own spelling (authoredBackendTheLaunchDecides), since nothing here
// resolves what that launch has yet to supply. Of those, the ones that resolve
// to nothing do leave the route to the workflow default, and reach it here; a
// `${VAR}` the host answers names the node's route, and the default is a field
// the node never reaches.
func (x *Executor) routeSpellingResolvesToARoute(raw string) bool {
	return !ir.BackendFallsThrough(x.programExecutor().ResolveRoutingField(raw))
}

// EffectiveFallbackBackends answers the engine's fallback-route seam for a dry
// run, from the same program-only executor and the same vars as the backend
// seam beside it. So on a launch the program fully specifies, `validate --exec`
// reads a `backend: "{{vars.fb}}"` route, and a route's `when:` gate, the way
// the run it simulates will — an executor that cannot answer leaves admission
// reading those routes from the IR, where a template bounds nothing: the dry
// run would refuse a fan-out the run admits.
//
// Those vars are the PROGRAM's — its declared defaults, and what the caller
// supplied — plus a shape this package invents for a var that has neither
// (VarValue). Where it invented one, the routes are read as their AUTHOR wrote
// them (authoredFallbackRoutes): the launch decides a template and a gate, the
// shape this package seeded is no run's value, and a verdict taken on it is a
// verdict about no run. It is the same pessimistic reading an executor that
// cannot answer the seam at all leaves standing, and it is this surface's whole
// guarantee, because the dry run's two passes cover a free-form var's domain
// with one shape: resolved from that shape, `backend: "{{vars.fb}}"` reads as
// the backend "x" and a route gated on `vars.rescue == 'on'` as one no run
// takes, and `validate --exec` would go CLEAN on a fan-out `--var rescue=on`
// has the engine refuse at the router.
func (x *Executor) EffectiveFallbackBackends(node ir.Node) []string {
	if x.launchDecidesTheProgramsRoutes() {
		return x.programExecutor().EffectiveFallbackBackends(node)
	}
	return x.authoredFallbackRoutes(node)
}

// launchDecidesTheProgramsRoutes reports whether every var this program
// declares carries a value something before the run decided: the author's own
// default, or a value the caller supplied. There — and only there — this dry
// run's vars are a run's vars, so every seam it answers is the executor's own
// answer and the verdict an operator reads before launching is the verdict
// that launch takes.
//
// A var with neither is seeded with a shape of its type (launchInputs,
// VarValue) and nothing before the launch decides the value, so a verdict
// taken on that shape is a verdict about no run: the two passes cover a
// free-form var's domain with one value, and `--var rescue=on` is a launch the
// program allows and no pass reads. Without the program in hand nothing is
// known, which reads as undecided.
//
// WHICH vars a routing field reads decides nothing here, deliberately: no
// parsing of an expression's shape admits a fan-out. That question has no sound
// cheap answer — a gate spelled `vars['x']` reads the vars namespace WHOLE, so
// the vars it may read are every var the program declares, and a field resting
// on the program's own default sits beside a gate resting on the launch — and
// any answer to it moves with a spelling its author is free to change. What the
// coarser rule costs is said where it is incurred: the refusal names the vars
// the launch must decide, and the `--var` that ends the pessimistic reading
// (UndecidedRouteReading).
func (x *Executor) launchDecidesTheProgramsRoutes() bool {
	return x.wf != nil && len(x.varsTheLaunchMustDecide()) == 0
}

// varsTheLaunchMustDecide are the vars this program declares with no default
// that the caller supplied no value for, sorted — the ones nothing before the
// launch decides. This pass reads a shape of each where it can produce one and
// nothing at all where it cannot (launchInputs); neither is a value a run would
// hold.
func (x *Executor) varsTheLaunchMustDecide() []string {
	if x.wf == nil {
		return nil
	}
	var out []string
	for name, v := range x.wf.Vars {
		if _, supplied := x.given[name]; supplied || v == nil || v.HasDefault {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// fieldIsTemplated reports whether a routing field carries a template span —
// the one part of such a field the LAUNCH decides, where a name is the
// program's own and a `${VAR}` reference is the host's. It is the reading
// admission takes of the answer too (pkg/runtime reads a surviving `{{` as an
// unresolved route), so the two cannot part on what counts as resolved.
func fieldIsTemplated(raw string) bool {
	return strings.Contains(raw, "{{")
}

// authoredFallbackRoutes is the reading taken on a launch the program does not
// fully specify: a node's `fallbacks:` routes as their AUTHOR wrote them,
// pessimistic in the two places the launch is what decides them.
//
//   - a route's `when:` gate is NOT evaluated: the launch may make it true, so
//     every route counts — the reading a dry run owes a gate no value of its
//     own decides.
//   - a `backend:` carrying a template keeps the author's spelling, which
//     admission reads as the unresolved route it is. Every other spelling is
//     resolved where the program and the host decide it (ResolveRoutingField,
//     the reading every routing field takes at dispatch), so the route is the
//     backend the program says it is, named as the run will name it — quoting
//     `${VAR:-claw}` as if it were a backend names a route no registry has. A
//     spelling that resolves to nothing (`${VAR}` unset) inherits the node's
//     backend, as it does at dispatch, so it adds no route to read.
//
// A route that executes nothing (`action: skip`) or names no backend of its own
// resolves none either way. The node's own route is out of the answer, as it is
// out of the executor's (model.ClawExecutor.EffectiveFallbackBackends): a
// `fallbacks:` entry that names it to vary a credential leaves nothing for a
// reader of this answer to judge.
func (x *Executor) authoredFallbackRoutes(node ir.Node) []string {
	var out []string
	for _, r := range x.authoredRoutes(node) {
		out = append(out, r.backend)
	}
	return out
}

// authoredRoute is one route of the answer above, with what about it the LAUNCH
// decides: the two spellings the reading is pessimistic about, carried on the
// route the reading PRODUCED rather than on the `fallbacks:` entry it came
// from.
//
// The distinction is the one the refusal's clause rests on: an entry whose
// route the answer drops — a gated route onto the node's own backend, a second
// route onto a backend an earlier one already named — costs this reading
// nothing, so a refusal saying the launch moved it would name a reading the
// verdict did not take.
type authoredRoute struct {
	backend   string
	templated bool
	gated     bool
}

// authoredRoutes is authoredFallbackRoutes with that provenance kept.
func (x *Executor) authoredRoutes(node ir.Node) []authoredRoute {
	llm, ok := node.(ir.LLMNode)
	if !ok {
		return nil
	}
	program := x.programExecutor()
	own := x.EffectiveBackendName(node)
	at := map[string]int{}
	var out []authoredRoute
	for _, fb := range llm.GetFallbacks() {
		raw := strings.TrimSpace(fb.Backend)
		if fb.Action == ir.FallbackActionSkip || raw == "" {
			continue
		}
		b, templated := raw, fieldIsTemplated(raw)
		if !templated {
			if b = program.ResolveRoutingField(raw); b == "" {
				continue
			}
		}
		if b == own {
			continue
		}
		gated := strings.TrimSpace(fb.When) != ""
		if i, dup := at[b]; dup {
			// Two entries onto ONE backend: the answer carries that backend
			// once, and what the launch decides about it is a property of the
			// entries together — never of the one its author happened to write
			// first. An ungated entry beside a gated one is a route every
			// launch takes, so the presence of that backend rests on no gate;
			// a templated spelling anywhere is one a launch resolves.
			out[i].gated = out[i].gated && gated
			out[i].templated = out[i].templated || templated
			continue
		}
		at[b] = len(out)
		out = append(out, authoredRoute{backend: b, templated: templated, gated: gated})
	}
	return out
}

// UndecidedRouteReading says what this dry run read pessimistically about where
// a node runs, and which `--var` ends that reading — and "" where the launch
// decides every routing field the node reaches.
//
// What it promises is the READING, never the verdict: supplying the vars it
// names has the dry run resolve the node's routing fields as the run will, and
// the verdict that follows is the executor's own — which may be the same
// refusal, for a program that is refused on every launch. The two states are
// what an author needs and all this seam can honestly offer: a refusal
// CARRYING the clause is not the run's verdict, one without it is. Promising
// the refusal would lift is a promise a per-node reading cannot keep — the
// reason beside the clause names ONE route, and the node's other routes decide
// nothing about it.
//
// It answers the engine's wording seam (runtime.UndecidedRouteReporter), read
// wherever a refusal names a route. What an author can act on in these cells is
// not their program — the route may be exactly the one they meant — but the
// launch the verdict was taken on, and a refusal naming a route they have no
// reason to change sends them to `--no-strict` or to a `readonly:` they do not
// mean, which is how a static gate stops being read at all.
//
// The two readings it reports are the two the rule takes
// (authoredFallbackRoutes, authoredBackendTheLaunchDecides), each named only
// where this node carries the field it rests on.
func (x *Executor) UndecidedRouteReading(node ir.Node) string {
	undecided := x.varsTheLaunchMustDecide()
	if len(undecided) == 0 {
		return ""
	}
	templated := x.authoredBackendTheLaunchDecides(node) != ""
	gated := false
	for _, r := range x.authoredRoutes(node) {
		templated = templated || r.templated
		gated = gated || r.gated
	}
	var readings []string
	if templated {
		readings = append(readings, "leaves a templated routing field unresolved")
	}
	if gated {
		readings = append(readings, "counts a gated route as one every launch takes")
	}
	if len(readings) == 0 {
		return ""
	}
	names := make([]string, 0, len(undecided))
	flags := make([]string, 0, len(undecided))
	for _, name := range undecided {
		names = append(names, "`"+name+"`")
		flags = append(flags, "--var "+name+"=…")
	}
	return fmt.Sprintf("the launch supplies no value for %s, so this verdict %s — pass `%s` and it reads the routes the run takes",
		strings.Join(names, ", "), strings.Join(readings, " and "), strings.Join(flags, " "))
}

func (x *Executor) programExecutor() *model.ClawExecutor {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.program == nil {
		p := model.NewProgramExecutor(x.wf)
		p.SetVars(x.vars)
		x.program = p
	}
	return x.program
}

// The engine reaches these seams through OPTIONAL type assertions, so an
// executor that forgets a method still compiles as one — and a forgotten seam
// costs the reading it answers: for the tool surface and the backend the worst
// case, where every agent and judge the simulation runs that is not
// `readonly:` counts as writing and its parallel fan-outs are refused; for a
// node's `fallbacks:` routes the IR's own reading of them, where a route
// spelled as a template bounds nothing. Asserted here, beside the
// implementation, so a rename breaks the build rather than a verdict — and
// against the EXPORTED seams, not structural copies: a copy catches a rename
// and misses the seam gaining a term.
//
// The wording seam is pinned with them: what a forgotten UndecidedRouteReading
// costs is a refusal that names a route read pessimistically and says nothing
// about the `--var` that ends that reading — an author sent to change a
// program that is not wrong.
// pkg/runtime cannot assert any of it for us: dryrun imports it, not the other
// way round.
var (
	_ runtime.EffectiveToolSurfaceResolver = (*Executor)(nil)
	_ runtime.EffectiveBackendResolver     = (*Executor)(nil)
	_ runtime.EffectiveFallbackResolver    = (*Executor)(nil)
	_ runtime.UndecidedRouteReporter       = (*Executor)(nil)
)

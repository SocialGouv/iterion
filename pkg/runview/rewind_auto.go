package runview

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// ErrRewindNoSourceRecorded is returned when --auto is asked of a run
// launched before the workflow source was persisted (or whose source
// could not be captured). Callers map it to 400 and point at --node.
var ErrRewindNoSourceRecorded = errors.New("runview: rewind: this run recorded no workflow source, so the edit cannot be located")

// ErrRewindNoChange is returned when --auto finds no difference between
// the recorded source and the current one, or when every difference
// lands on a node the run never executed.
var ErrRewindNoChange = errors.New("runview: rewind: no change affects a node this run executed")

// ErrRewindAmbiguous is returned when the edit touches several
// independent branches, so there is no single earliest node to rewind
// to. Callers map it to 400 and name the candidates.
var ErrRewindAmbiguous = errors.New("runview: rewind: the edit affects independent branches")

// DeclChange is one differing top-level declaration between the source a
// run executed and the source on disk now.
type DeclChange struct {
	// Kind is the declaration kind: agent, judge, router, human, tool,
	// compute, subbot, emit, wait, await_answers, prompt, schema, cursor,
	// supervisor, mcp_server, vars, presets, attachments, secrets, edge,
	// or workflow.
	Kind string `json:"kind"`
	// Name identifies the declaration (node id, prompt name, "a -> b" for
	// an edge, the workflow name for workflow-level settings).
	Name string `json:"name"`
	// Change is "modified", "added", or "removed".
	Change string `json:"change"`
}

func (c DeclChange) String() string { return c.Kind + " " + c.Name + " (" + c.Change + ")" }

// resolveAutoPivot diffs the source a run executed against the source on
// disk now, and returns the node to rewind to: the earliest node the run
// actually executed that the edit affects.
//
// This is what makes the bot-development loop one step instead of two —
// "I changed the prompt of implement" no longer has to be translated by
// hand into "--node implement", and an edited edge or a shared prompt
// resolves to the right pivot without the operator tracing references.
//
// Detection is declaration-granular and deliberately errs toward
// reporting MORE nodes than strictly necessary: a false positive costs
// re-executing a node that did not need it, while a false negative would
// test the new configuration against stale downstream state — the exact
// failure this feature exists to prevent.
func resolveAutoPivot(oldSrc, newSrc string, wf *ir.Workflow, executed map[string]bool) (string, []DeclChange, error) {
	if strings.TrimSpace(oldSrc) == "" {
		return "", nil, ErrRewindNoSourceRecorded
	}
	oldFile, err := parseForDiff(oldSrc, "<recorded>")
	if err != nil {
		return "", nil, fmt.Errorf("parse the source this run executed: %w", err)
	}
	newFile, err := parseForDiff(newSrc, "<current>")
	if err != nil {
		return "", nil, fmt.Errorf("parse the current source: %w", err)
	}

	changes := diffDecls(declFingerprints(oldFile), declFingerprints(newFile))
	if len(changes) == 0 {
		return "", nil, fmt.Errorf("%w: the workflow source is unchanged", ErrRewindNoChange)
	}

	impacted := impactedNodes(changes, newFile, wf)
	candidates := make([]string, 0, len(impacted))
	for id := range impacted {
		if executed[id] {
			candidates = append(candidates, id)
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return "", changes, fmt.Errorf("%w: changed %s", ErrRewindNoChange, summarizeChanges(changes))
	}

	earliest := earliestCandidates(wf, candidates)
	if len(earliest) > 1 {
		return "", changes, fmt.Errorf("%w: %s are on independent branches — pick one with --node",
			ErrRewindAmbiguous, strings.Join(earliest, ", "))
	}
	if len(earliest) == 0 {
		// Unreachable: earliestCandidates collapses cycles rather than
		// eliminating them, so a non-empty candidate set always yields at
		// least one survivor. Guarded anyway — indexing [0] here is what
		// turned a graph-shape edge case into a panic once already.
		return "", changes, fmt.Errorf("%w: could not order %s", ErrRewindAmbiguous, strings.Join(candidates, ", "))
	}
	return earliest[0], changes, nil
}

// parseForDiff parses a source into an AST, refusing on hard parse
// errors. The recorded source parsed once already (the run executed), so
// a failure here means the CURRENT file is broken — worth surfacing
// before the operator resumes into it.
func parseForDiff(src, label string) (*ast.File, error) {
	pr := parser.Parse(label, src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			return nil, fmt.Errorf("%s", d.Error())
		}
	}
	if pr.File == nil {
		return nil, fmt.Errorf("no workflow found")
	}
	return pr.File, nil
}

// declFingerprints canonicalises every top-level declaration to a string
// keyed by "<kind>:<name>".
//
// The canonical form is the AST JSON encoder applied to a file holding
// that single declaration. Reusing ast.MarshalFile rather than hand-rolling
// a fingerprint matters twice over: it stays faithful as the DSL grows a
// field (a hand-rolled one silently stops noticing), and it omits Span
// fields, so inserting a line at the top of a .bot does not make every
// declaration below it look edited.
func declFingerprints(f *ast.File) map[string]string {
	out := map[string]string{}
	put := func(kind, name string, file *ast.File) {
		b, err := ast.MarshalFile(file)
		if err != nil {
			// An unencodable declaration is treated as "always changed"
			// rather than "never changed": erring toward re-execution is
			// the safe direction.
			out[kind+":"+name] = fmt.Sprintf("<unencodable %d>", len(out))
			return
		}
		out[kind+":"+name] = string(b)
	}

	for _, d := range f.Agents {
		put("agent", d.Name, &ast.File{Agents: []*ast.AgentDecl{d}})
	}
	for _, d := range f.Judges {
		put("judge", d.Name, &ast.File{Judges: []*ast.JudgeDecl{d}})
	}
	for _, d := range f.Routers {
		put("router", d.Name, &ast.File{Routers: []*ast.RouterDecl{d}})
	}
	for _, d := range f.Humans {
		put("human", d.Name, &ast.File{Humans: []*ast.HumanDecl{d}})
	}
	for _, d := range f.Tools {
		put("tool", d.Name, &ast.File{Tools: []*ast.ToolNodeDecl{d}})
	}
	for _, d := range f.Computes {
		put("compute", d.Name, &ast.File{Computes: []*ast.ComputeDecl{d}})
	}
	for _, d := range f.Subbots {
		put("subbot", d.Name, &ast.File{Subbots: []*ast.SubbotDecl{d}})
	}
	for _, d := range f.Emits {
		put("emit", d.Name, &ast.File{Emits: []*ast.EmitDecl{d}})
	}
	for _, d := range f.Waits {
		put("wait", d.Name, &ast.File{Waits: []*ast.WaitDecl{d}})
	}
	for _, d := range f.AwaitAnswers {
		put("await_answers", d.Name, &ast.File{AwaitAnswers: []*ast.AwaitAnswersDecl{d}})
	}
	for _, d := range f.Prompts {
		put("prompt", d.Name, &ast.File{Prompts: []*ast.PromptDecl{d}})
	}
	for _, d := range f.Schemas {
		put("schema", d.Name, &ast.File{Schemas: []*ast.SchemaDecl{d}})
	}
	for _, d := range f.Cursors {
		put("cursor", d.Name, &ast.File{Cursors: []*ast.CursorDecl{d}})
	}
	for _, d := range f.Supervisors {
		put("supervisor", d.Name, &ast.File{Supervisors: []*ast.SupervisorDecl{d}})
	}
	for _, d := range f.MCPServers {
		put("mcp_server", d.Name, &ast.File{MCPServers: []*ast.MCPServerDecl{d}})
	}
	if f.Vars != nil {
		put("vars", "", &ast.File{Vars: f.Vars})
	}
	if f.Presets != nil {
		put("presets", "", &ast.File{Presets: f.Presets})
	}
	if f.Attachments != nil {
		put("attachments", "", &ast.File{Attachments: f.Attachments})
	}
	if f.Secrets != nil {
		put("secrets", "", &ast.File{Secrets: f.Secrets})
	}

	// The workflow block splits in two: each edge on its own (so an edge
	// edit blames its SOURCE node instead of the whole graph), and the
	// remaining workflow-level settings as one unit.
	for _, w := range f.Workflows {
		if w == nil {
			continue
		}
		for _, e := range w.Edges {
			if e == nil {
				continue
			}
			put("edge", edgeKey(w.Name, e), &ast.File{Workflows: []*ast.WorkflowDecl{{Name: w.Name, Edges: []*ast.Edge{e}}}})
		}
		shell := *w
		shell.Edges = nil
		put("workflow", w.Name, &ast.File{Workflows: []*ast.WorkflowDecl{&shell}})
	}
	return out
}

// edgeKey names an edge stably across edits. Two edges can share
// (from, to) — a conditional pair, or an else branch — so the guard
// participates in the identity; otherwise editing one would read as
// editing both, and both source nodes would land in the impacted set.
func edgeKey(workflow string, e *ast.Edge) string {
	// Semantic fields ONLY: WhenClause/LoopClause/ForeachClause each carry
	// a Span, and interpolating the whole struct would move the key every
	// time a line is inserted above — making every edge read as
	// removed-and-re-added on any edit anywhere in the file.
	key := workflow + ":" + e.From + " -> " + e.To
	switch {
	case e.When != nil:
		switch {
		case e.When.Expr != "":
			key += " when " + e.When.Expr
		case e.When.Negated:
			key += " when not " + e.When.Condition
		default:
			key += " when " + e.When.Condition
		}
	case e.IsElse:
		key += " else"
	}
	if e.Loop != nil {
		key += " as " + e.Loop.Name
	}
	if e.Foreach != nil {
		key += " foreach " + e.Foreach.Name
	}
	return key
}

// diffDecls compares two fingerprint maps into a sorted change list.
func diffDecls(oldFP, newFP map[string]string) []DeclChange {
	var out []DeclChange
	for key, newVal := range newFP {
		oldVal, existed := oldFP[key]
		switch {
		case !existed:
			out = append(out, declChangeFromKey(key, "added"))
		case oldVal != newVal:
			out = append(out, declChangeFromKey(key, "modified"))
		}
	}
	for key := range oldFP {
		if _, still := newFP[key]; !still {
			out = append(out, declChangeFromKey(key, "removed"))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func declChangeFromKey(key, change string) DeclChange {
	kind, name, _ := strings.Cut(key, ":")
	return DeclChange{Kind: kind, Name: name, Change: change}
}

// nodeDeclKinds are the declaration kinds that ARE graph nodes, so a
// change maps straight onto the node of the same name.
var nodeDeclKinds = map[string]bool{
	"agent": true, "judge": true, "router": true, "human": true,
	"tool": true, "compute": true, "subbot": true,
	"emit": true, "wait": true, "await_answers": true,
}

// impactedNodes maps a change list onto the graph nodes it affects.
func impactedNodes(changes []DeclChange, newFile *ast.File, wf *ir.Workflow) map[string]bool {
	impacted := map[string]bool{}
	fingerprints := declFingerprints(newFile)

	for _, c := range changes {
		switch {
		case nodeDeclKinds[c.Kind]:
			impacted[c.Name] = true

		case c.Kind == "edge":
			// An edge belongs to the node that routes over it: that node
			// re-runs and re-selects. "wf:a -> b when ok" → "a".
			_, rest, _ := strings.Cut(c.Name, ":")
			if from, _, ok := strings.Cut(rest, " -> "); ok {
				impacted[strings.TrimSpace(from)] = true
			}

		case c.Kind == "workflow" || c.Kind == "vars" || c.Kind == "presets" ||
			c.Kind == "attachments" || c.Kind == "secrets":
			// Workflow-scope settings (budget, sandbox, permission, entry,
			// vars…) can reach any node, so the only safe pivot is the
			// entry — the whole run is affected.
			if wf.Entry != "" {
				impacted[wf.Entry] = true
			}

		default:
			// A shared declaration (prompt, schema, cursor, supervisor,
			// mcp_server): every node referencing it by name is affected.
			// The reference shows up as a quoted JSON string in that
			// node's canonical form, which covers system/user/input/output
			// and any future field carrying a name — no per-field
			// enumeration to keep in sync with the DSL.
			needle := `"` + c.Name + `"`
			for key, fp := range fingerprints {
				kind, name, _ := strings.Cut(key, ":")
				if !nodeDeclKinds[kind] {
					continue
				}
				if strings.Contains(fp, needle) {
					impacted[name] = true
				}
			}
		}
	}

	// Drop anything that is not a node in the compiled graph (a removed
	// node, or a shared-decl name that coincides with nothing).
	for id := range impacted {
		if _, ok := wf.Nodes[id]; !ok {
			delete(impacted, id)
		}
	}
	return impacted
}

// earliestCandidates keeps the candidates that no OTHER candidate can
// reach — the upstream-most edits. Rewinding to one of those replays
// every other affected node downstream of it, so the whole edit is
// tested in one pass.
//
// More than one survivor means the edits sit on branches that cannot
// reach each other (a fan-out), where no single pivot covers them all.
func earliestCandidates(wf *ir.Workflow, candidates []string) []string {
	if len(candidates) == 0 {
		return nil
	}
	fwdAdj, revAdj := adjacency(wf, false), adjacency(wf, true)
	ancestors := make(map[string]map[string]bool, len(candidates))
	descendants := make(map[string]map[string]bool, len(candidates))
	for _, c := range candidates {
		ancestors[c] = reachable(c, revAdj)
		descendants[c] = reachable(c, fwdAdj)
	}

	var out []string
	for _, c := range candidates {
		dominated := false
		for _, other := range candidates {
			// STRICTLY upstream: `other` reaches c, and c cannot reach
			// back. The second half is what makes loops work. Without it,
			// two edited nodes on a cycle are each other's ancestor, so
			// both get eliminated — which used to leave the survivor set
			// empty (a panic) and, worse, could silently elect an
			// unrelated third candidate while dropping the loop edit
			// entirely.
			if other != c && ancestors[c][other] && !descendants[c][other] {
				dominated = true
				break
			}
		}
		if !dominated {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	if len(out) <= 1 {
		return out
	}
	// Survivors that can all reach each other are one cycle, not
	// independent branches: replaying any of them replays the loop. Elect
	// the one execution reaches first so the replay covers every edit.
	if mutuallyReachable(out, descendants) {
		return []string{nearestToEntry(wf, out, fwdAdj)}
	}
	return out
}

// mutuallyReachable reports whether every node can reach every other —
// i.e. they belong to one strongly-connected component.
func mutuallyReachable(nodes []string, descendants map[string]map[string]bool) bool {
	for _, a := range nodes {
		for _, b := range nodes {
			if a != b && !descendants[a][b] {
				return false
			}
		}
	}
	return true
}

// nearestToEntry picks the candidate the workflow reaches in the fewest
// hops from its entry, breaking ties by name so the choice is stable
// across runs.
func nearestToEntry(wf *ir.Workflow, candidates []string, fwdAdj map[string][]string) string {
	dist := map[string]int{wf.Entry: 0}
	queue := []string{wf.Entry}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, next := range fwdAdj[id] {
			if _, seen := dist[next]; seen {
				continue
			}
			dist[next] = dist[id] + 1
			queue = append(queue, next)
		}
	}
	best, bestDist := candidates[0], 1<<30
	for _, c := range candidates {
		d, ok := dist[c]
		if !ok {
			continue // unreachable from entry; never preferred
		}
		if d < bestDist || (d == bestDist && c < best) {
			best, bestDist = c, d
		}
	}
	return best
}

// summarizeChanges renders a change list for an error message.
func summarizeChanges(changes []DeclChange) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, c.String())
	}
	return strings.Join(parts, ", ")
}

// fanOutRouterFor returns the outermost fan-out router whose body
// contains nodeID, or "" when the node sits outside any fan-out region.
//
// Why a rewind must promote to it: the checkpoint holds ONE output per
// node id (convergence merges branches last-write-wins), so N parallel
// executions of a body node collapse to a single entry — there is no
// per-branch state to rewind to. Re-executing "all of them" is only
// expressible as "re-run the fan-out", and the fan-out is orchestrated by
// the router, not by the body node. Anchoring on the body node instead
// would replay it ONCE, linearly, with no `each` context — silently
// testing one iteration instead of N.
//
// Outermost, not nearest: a nested inner router re-run on its own would
// itself lack the outer iteration's context.
func fanOutRouterFor(wf *ir.Workflow, nodeID string) string {
	bodies := map[string]map[string]bool{}
	for id, node := range wf.Nodes {
		r, ok := node.(*ir.RouterNode)
		if !ok || (r.RouterMode != ir.RouterFanOutAll && r.RouterMode != ir.RouterFanOutEach) {
			continue
		}
		bodies[id] = fanOutBody(wf, id)
	}
	var containing []string
	for routerID, body := range bodies {
		if body[nodeID] {
			containing = append(containing, routerID)
		}
	}
	if len(containing) == 0 {
		return ""
	}
	sort.Strings(containing)
	// The outermost is the one no OTHER fan-out router's body contains.
	for _, candidate := range containing {
		nested := false
		for _, other := range containing {
			if other != candidate && bodies[other][candidate] {
				nested = true
				break
			}
		}
		if !nested {
			return candidate
		}
	}
	return containing[0]
}

// fanOutBody walks forward from a fan-out router and returns the nodes
// that execute inside its branches. The walk stops AT the convergence —
// any node declaring `await:` — which is the boundary where branches
// merge back into the main path; the convergence node itself is outside
// the body, since it runs once rather than per-branch.
func fanOutBody(wf *ir.Workflow, routerID string) map[string]bool {
	adj := adjacency(wf, false)
	body := map[string]bool{}
	queue := append([]string(nil), adj[routerID]...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if body[id] || id == routerID {
			continue
		}
		node, ok := wf.Nodes[id]
		if !ok {
			continue
		}
		if ir.NodeAwaitMode(node) != ir.AwaitNone {
			// Convergence: the boundary, excluded from the body and not
			// expanded past.
			continue
		}
		body[id] = true
		queue = append(queue, adj[id]...)
	}
	return body
}

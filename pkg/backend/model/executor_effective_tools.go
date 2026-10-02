package model

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// EffectiveToolNames reports the tool names the runtime puts on
// delegate.Task.AllowedTools for a node: the author's `tools:` list after the
// runtime's own opt-in appends (`ask_user` under `interaction:`, `todo_write`
// on claw, the file trio under `auto_memory:`, the `agent` subagent tool under
// ultracode on claw, the board/runs MCP tools under `capabilities:`), unioned
// over EVERY route the node may take.
//
// It is NOT the node's whole tool surface, and a caller must not read it as
// one: ambient `mcp:` servers are spliced in later for claw, and the ask_user
// /board/runs wiring appends its own extras after this. What it answers is
// "which names can this node's DECLARATION not bound", which is the question
// pre-run admission asks.
//
// It exists so the engine's pre-run analyses can ask what a node holds instead
// of re-deriving it from the IR. Re-deriving is what makes a guard wrong here:
// `auto_memory` resolves through a launch override, the workflow, and
// ITERION_AUTO_MEMORY, and the ultracode mode resolves through the per-node
// model override — none of which is in the IR. This is the same seam, and the
// same reason, as EffectiveBackendName.
//
// The routes are walked in BOTH directions on purpose. The appends are decided
// per route (buildTask is re-run for a `fallbacks:` element that changes the
// backend), so a claude_code node with a `backend: "claw"` route picks up
// claw's appends when it falls through, and a claw node with a CLI route loses
// the list's meaning entirely. A caller deciding ONCE, before the run, needs
// the union.
//
// A nil/empty answer means "nothing to add" — the caller then reads the
// declared list, which is what it did before this seam existed.
func (e *ClawExecutor) EffectiveToolNames(node ir.Node, mayEscalateToUltracode bool) []string {
	if e == nil || node == nil {
		return nil
	}
	f, err := extractBackendFields(node)
	if err != nil {
		return nil
	}
	caps := f.capabilities
	if caps == nil {
		caps = e.wfCapabilities
	}
	// An incoming edge may raise this node to ultracode at dispatch:
	// `_reasoning_effort` is a mapped input and `ultracode` IS a member of
	// ir.ValidReasoningEfforts (docs/ultracode.md documents that edge). That
	// input does not exist yet here, so the caller — which holds the graph —
	// says whether any edge into this node can carry it, and the surface is
	// then taken over BOTH values. Admission is decided once, before the run:
	// a decision that cannot see an input takes the pessimistic side of it,
	// but only where the input can actually arrive. The launch-time override
	// is read either way.
	ultracode := e.effortForNode(node, f.reasoningEffort, nil) == "ultracode"
	efforts := []bool{ultracode}
	if mayEscalateToUltracode && !ultracode {
		efforts = append(efforts, true)
	}

	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, backend := range e.routeBackends(node) {
		for _, ultra := range efforts {
			for _, name := range e.assembleEffectiveTools(f, backend, caps, ultra) {
				add(name)
			}
		}
	}
	return out
}

// routeBackends lists every backend a node may run on: the resolved primary,
// plus each `fallbacks:` element that names one of its own. An element with an
// empty Backend inherits the primary and adds nothing.
func (e *ClawExecutor) routeBackends(node ir.Node) []string {
	primary := e.resolveBackendName(node)
	out := []string{primary}
	seen := map[string]bool{primary: true}
	for _, el := range e.resolveChain(node) {
		if el.Backend == "" || seen[el.Backend] {
			continue
		}
		seen[el.Backend] = true
		out = append(out, el.Backend)
	}
	return out
}

// NewProgramExecutor builds an executor that answers questions about a PROGRAM
// and never about the host it is asked on.
//
// The engine's pre-run analyses need one seam — "which tools will this node
// hold" — and a dry run needs the same answer as a real run, or `iterion
// validate --exec --strict` and `iterion run` disagree about one file. But a
// real executor resolves its opt-ins through the host as well as the program:
// ITERION_AUTO_MEMORY, ITERION_DEFAULT_BACKEND, a credential probe. Reading
// those in a static check makes the verdict depend on the machine that runs
// it, and refuses a file a run with `--auto-memory off` would admit.
//
// So this one reads the program: the node's and the workflow's own
// `auto_memory:`, `capabilities:` and `default_backend:`, and the vars a caller
// hands it through SetVars — never ITERION_AUTO_MEMORY, ITERION_DEFAULT_BACKEND
// or the credential probe (a `${VAR:-x}` backend is expanded from the
// environment, as every reading of the IR expands it). A node that names no
// backend resolves to none, which every pre-run analysis already reads as "the
// IR is all there is". A run that carries an override is judged again, by the
// engine, against what that run actually holds.
func NewProgramExecutor(wf *ir.Workflow) *ClawExecutor {
	if wf == nil {
		return &ClawExecutor{staticProgramOnly: true}
	}
	return &ClawExecutor{
		staticProgramOnly: true,
		defaultBackend:    wf.DefaultBackend,
		wfAutoMemory:      wf.AutoMemory,
		wfCapabilities:    wf.Capabilities,
	}
}

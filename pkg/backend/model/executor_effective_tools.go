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
	// Answered before the routes are resolved: resolving them reads the node
	// (its id and kind, for a launch override), so the documented nil arm has
	// to be the first thing either method does.
	if e == nil || node == nil {
		return nil
	}
	return e.EffectiveToolNamesOnRoutes(node, mayEscalateToUltracode, e.routeBackends(node))
}

// EffectiveToolNamesOnRoutes is EffectiveToolNames over a route list its CALLER
// resolved — the appends, and the union over routes, taken here exactly as the
// method above takes them.
//
// It exists for one caller: a reading that answers the engine's fallback seam
// itself must not have the appends taken over a SECOND reading of the node's
// routes. pkg/dryrun does — it answers a route the dry run's own program
// resolves, and the author's spelling for one that would rest on a value it
// invented — and a dry run that let this method resolve the routes again
// reported a fan-out clean on the surface of routes its own admission counted
// (`validate --exec` clean, `iterion run` refused at the router).
//
// A nil answer where the node is not an LLM node, and the empty list where the
// caller resolved no route, are the answers of the method above, for the same
// reason: "nothing to add", read by the caller as the declared list.
func (e *ClawExecutor) EffectiveToolNamesOnRoutes(node ir.Node, mayEscalateToUltracode bool, routes []string) []string {
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
	for _, backend := range routes {
		for _, ultra := range efforts {
			for _, name := range e.assembleEffectiveTools(f, backend, caps, ultra) {
				add(name)
			}
		}
	}
	return out
}

// routeBackends lists every backend a node may run on: the resolved primary,
// plus the `fallbacks:` routes EffectiveFallbackBackends resolves — one
// reading of the chain for this question and for admission's, so neither can
// drift from what dispatch walks.
//
// That holds for THIS executor's readings. A caller whose own answer to the
// fallback seam is not this one — pkg/dryrun's, which keeps the author's
// spelling for a route resting on a value it invented — owes the union the
// routes it answers, through EffectiveToolNamesOnRoutes.
func (e *ClawExecutor) routeBackends(node ir.Node) []string {
	primary := e.resolveBackendName(node)
	out := []string{primary}
	for _, route := range e.EffectiveFallbackBackends(node) {
		if route != primary {
			out = append(out, route)
		}
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
//
// `vars:` is the one input a CALLER owes before asking either seam, and the
// only program-carried field this constructor does not seed from the workflow
// (NewClawExecutor does, from the declared defaults). Two readings were
// available and neither belongs here: the host one resolves a default spelled
// `${PROJECT_DIR}` against the machine, which is what this type exists not to
// do, and the as-written one would RELAX a pre-run guard — measured, a route
// spelled `{{vars.b}}` with a default naming claw or codex is read as
// unresolved today, so admission refuses it, and resolving it admits the pair
// (TestTheProgramOnlyReadingAnswersTheProgramsOwnAppends carries the row).
// Pre-run analyses take the pessimistic side of what they cannot resolve;
// pkg/dryrun calls SetVars with the run's own values, which nils the cached
// executor, so `iterion validate --exec` reads them resolved.
func NewProgramExecutor(wf *ir.Workflow) *ClawExecutor {
	if wf == nil {
		return &ClawExecutor{staticProgramOnly: true}
	}
	return &ClawExecutor{
		staticProgramOnly: true,
		defaultBackend:    wf.DefaultBackend,
		wfAutoMemory:      wf.AutoMemory,
		wfCapabilities:    wf.Capabilities,
		// The image-attachment append is decided by the PROGRAM — a
		// workflow's `attachments:` and the prompt body that references one —
		// so the seams read it here too. On every backend but claw it puts
		// `read_image` on the node's effective list, and codex sandboxes that
		// name workspace-write: a reading that dropped these two fields would
		// admit a parallel fan-out the engine refuses.
		prompts:      wf.Prompts,
		imageAttachs: imageAttachmentNames(wf),
	}
}

// imageAttachmentNames names the workflow's image-typed attachments — the one
// place either executor builds that set, so the program-only reading and the
// host's cannot answer a different effective surface for the same file.
func imageAttachmentNames(wf *ir.Workflow) map[string]bool {
	names := map[string]bool{}
	for name, a := range wf.Attachments {
		if a != nil && a.Type == ir.AttachmentImage {
			names[name] = true
		}
	}
	return names
}
